// Package netstack runs the userspace network stack for one --net run inside the sbxr process.
//
// The VM reaches the stack through one end of a datagram socket pair, one Ethernet frame per datagram.
// The stack serves DHCP and DNS on the gateway, forwards guest TCP and UDP flows that the egress policy allows, and publishes guest TCP ports on the host.
// It is built from gvisor-tap-vsock and gvisor packages with its own forwarders, because the library forwarders dial any destination and count nothing.
package netstack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const (
	nicID            = 1
	mtu              = 1500
	defaultMaxTCP    = 1024
	defaultMaxUDP    = 256
	tcpMaxInFlight   = 128
	dialTimeout      = 10 * time.Second
	udpIdleTimeout   = 90 * time.Second
	hostAddrInterval = 2 * time.Second
	closeTimeout     = 5 * time.Second
)

var (
	subnet     = netip.MustParsePrefix("192.168.127.0/24")
	gatewayIP  = netip.MustParseAddr("192.168.127.1")
	guestIP    = netip.MustParseAddr("192.168.127.2")
	gatewayMAC = "5a:94:ef:e4:0c:dd"
	guestMAC   = "5a:94:ef:e4:0c:ee"
	loopback   = netip.MustParseAddr("127.0.0.1")

	guestHWAddr   = mustParseMAC(guestMAC)
	gatewayHWAddr = mustParseMAC(gatewayMAC)
)

// DialFunc opens a host connection for a guest flow. It has the signature of [net.Dialer.DialContext].
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config describes the network of one run.
type Config struct {
	// AllowLAN allows RFC1918 and carrier-grade NAT destinations and the host's own interface addresses.
	AllowLAN bool

	// Publish lists the guest TCP ports to publish on the host.
	Publish []Publish

	// MaxTCP and MaxUDP cap the TCP connections and UDP flows of the run. Zero means 1024 and 256.
	MaxTCP int
	MaxUDP int

	// Logger receives debug logs, including the library logs. Nil discards them.
	Logger *slog.Logger

	// Dial opens host connections for allowed guest flows after the egress check. Nil uses [net.Dialer].
	// Tests set it to send public-looking destinations to local servers.
	Dial DialFunc

	// Hosts maps DNS names to addresses that the gateway DNS answers without asking the host resolver.
	// Names are lowercase without the trailing dot. Other query types for these names get an empty answer.
	// Tests use it, because they have no external DNS.
	Hosts map[string]netip.Addr

	// udpIdle, resolver, and nameservers override the UDP flow idle timeout, the host resolver, and the host nameservers in tests.
	udpIdle     time.Duration
	resolver    *net.Resolver
	nameservers []string
}

// Publish maps a host TCP address to a guest TCP port.
type Publish struct {
	// Host is an IPv4 address and port. An invalid address means 127.0.0.1, and port 0 picks a free port.
	Host  netip.AddrPort
	Guest uint16
}

// String formats p for logs, for example "127.0.0.1:8080 -> 80/tcp".
func (p Publish) String() string {
	return fmt.Sprintf("%s -> %d/tcp", p.Host, p.Guest)
}

// Network is the running network stack of one run.
type Network struct {
	logger *slog.Logger
	policy *policy
	dial   DialFunc
	tcp    limiter
	udp    limiter

	udpIdle time.Duration

	ctx    context.Context //nolint:containedctx // The stack lives until Close and cancels its flows through it.
	cancel context.CancelFunc

	dnsConns  chan net.Conn
	stack     *stack.Stack
	sw        *tap.Switch
	vmFile    *os.File
	vmConn    net.Conn
	services  []io.Closer
	listeners []net.Listener
	published []Publish

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// Start creates the socket pair and starts the stack, the gateway services, and the publish listeners.
// Cancelling ctx aborts all flows. Close must be called in every case.
func Start(ctx context.Context, cfg Config) (*Network, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	routeLibraryLogs(logger)

	publish, err := normalizePublish(cfg.Publish)
	if err != nil {
		return nil, err
	}

	//nolint:exhaustruct_v5 // start sets the remaining fields.
	n := &Network{
		logger:  logger,
		policy:  newPolicy(cfg.AllowLAN, publish),
		dial:    cfg.Dial,
		tcp:     newLimiter(cfg.MaxTCP, defaultMaxTCP),
		udp:     newLimiter(cfg.MaxUDP, defaultMaxUDP),
		udpIdle: cmp.Or(cfg.udpIdle, udpIdleTimeout),
	}
	n.ctx, n.cancel = context.WithCancel(ctx)
	n.dnsConns = make(chan net.Conn)

	if n.dial == nil {
		var d net.Dialer

		n.dial = d.DialContext
	}

	if err := n.start(cfg, publish); err != nil {
		return nil, errors.Join(err, n.Close())
	}

	for _, p := range n.published {
		logger.Debug("published " + p.String())
	}

	return n, nil
}

func (n *Network) start(cfg Config, publish []Publish) error {
	if err := n.policy.refreshHostAddrs(); err != nil {
		return err
	}

	hostFile, vmFile, err := socketpair()
	if err != nil {
		return err
	}

	n.vmFile = vmFile

	conn, err := net.FileConn(hostFile)
	_ = hostFile.Close()

	if err != nil {
		return fmt.Errorf("network socket: %w", err)
	}

	n.vmConn = &filterConn{Conn: conn, policy: n.policy}

	sw, pool, err := n.newStack()
	if err != nil {
		return err
	}

	if err := n.startServices(pool, cfg); err != nil {
		return err
	}

	for _, p := range publish {
		if err := n.listen(p); err != nil {
			return err
		}
	}

	n.wg.Go(func() { _ = sw.Accept(n.ctx, n.vmConn, types.VfkitProtocol) })
	n.wg.Go(n.refreshHostAddrs)

	return nil
}

func (n *Network) newStack() (*tap.Switch, *tap.IPPool, error) {
	pool, err := tap.NewIPPool(subnet.String())
	if err != nil {
		return nil, nil, fmt.Errorf("network address pool: %w", err)
	}

	if err := pool.Reserve(gatewayIP.String(), gatewayMAC); err != nil {
		return nil, nil, fmt.Errorf("network address pool: %w", err)
	}

	if err := pool.Reserve(guestIP.String(), guestMAC); err != nil {
		return nil, nil, fmt.Errorf("network address pool: %w", err)
	}

	ep, err := tap.NewLinkEndpoint(false, mtu, gatewayMAC, gatewayIP.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("network link: %w", err)
	}

	sw := tap.NewSwitch(false)
	n.sw = sw
	ep.Connect(sw)
	sw.Connect(ep)

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	n.stack = s

	if err := s.CreateNIC(nicID, ep); err != nil {
		return nil, nil, fmt.Errorf("network interface: %s", err)
	}

	gw := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpAddr(gatewayIP).WithPrefix()}
	if err := s.AddProtocolAddress(nicID, gw, stack.AddressProperties{}); err != nil {
		return nil, nil, fmt.Errorf("gateway address: %s", err)
	}

	// Spoofing and promiscuous mode let the gateway stack accept and answer traffic for every destination.
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, nil, fmt.Errorf("network interface: %s", err)
	}

	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, nil, fmt.Errorf("network interface: %s", err)
	}

	route, err := tcpip.NewSubnet(tcpAddr(subnet.Addr()), tcpip.MaskFromBytes(net.CIDRMask(subnet.Bits(), 32)))
	if err != nil {
		return nil, nil, fmt.Errorf("network route: %w", err)
	}

	s.SetRouteTable([]tcpip.Route{{Destination: route, NIC: nicID}})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, tcpMaxInFlight, n.forwardTCP).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s, n.forwardUDP).HandlePacket)

	return sw, pool, nil
}

func (n *Network) startServices(pool *tap.IPPool, cfg Config) error {
	dnsAddr := tcpip.FullAddress{NIC: nicID, Addr: tcpAddr(gatewayIP), Port: portDNS}

	dnsUDP, err := gonet.DialUDP(n.stack, &dnsAddr, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("dns: %w", err)
	}

	n.services = append(n.services, dnsUDP)

	h := newDNSHandler(n.ctx, cfg.Hosts, cfg.resolver, cfg.nameservers)
	udpSrv := &dns.Server{PacketConn: dnsUDP, Handler: h}
	tcpSrv := &dns.Server{
		Listener:    &connListener{conns: n.dnsConns, done: n.ctx.Done()},
		Handler:     h,
		IdleTimeout: func() time.Duration { return dnsTimeout },
	}

	gvCfg := &types.Configuration{
		MTU:              mtu,
		Subnet:           subnet.String(),
		GatewayIP:        gatewayIP.String(),
		DHCPStaticLeases: map[string]string{guestIP.String(): guestMAC},
	}

	dhcpSrv, err := dhcp.New(gvCfg, n.stack, pool)
	if err != nil {
		return fmt.Errorf("dhcp: %w", err)
	}

	n.services = append(n.services, dhcpSrv.Underlying)

	n.wg.Go(func() { _ = udpSrv.ActivateAndServe() })
	n.wg.Go(func() { _ = tcpSrv.ActivateAndServe() })
	n.wg.Go(func() { _ = dhcpSrv.Serve() })

	return nil
}

func (n *Network) refreshHostAddrs() {
	t := time.NewTicker(hostAddrInterval)
	defer t.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			if err := n.policy.refreshHostAddrs(); err != nil {
				n.logger.Debug("netstack: " + err.Error())
			}
		}
	}
}

// VMFile returns the VM end of the socket pair for krun_add_net_unixgram in fd mode.
// The frames carry no VFKT handshake, so the VM must not set NET_FLAG_VFKIT.
// The caller may close it after the VM process starts. Close closes it too.
func (n *Network) VMFile() *os.File { return n.vmFile }

// GuestMAC returns the MAC address that the VM must use, because DHCP has a static lease for it.
func (*Network) GuestMAC() net.HardwareAddr { return slices.Clone(guestHWAddr) }

// GuestIP returns the address that DHCP gives the guest.
func (*Network) GuestIP() netip.Addr { return guestIP }

// Published returns the bound publish addresses, with the real port when the request asked for port 0.
func (n *Network) Published() []Publish { return slices.Clone(n.published) }

// Close stops the listeners, the flows, the services, and the stack.
// It does not wait for blocked relays longer than a few seconds, so it never blocks the process exit.
func (n *Network) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}

	n.closed = true
	n.mu.Unlock()

	n.cancel()

	for _, ln := range n.listeners {
		_ = ln.Close()
	}

	if n.vmConn != nil {
		_ = n.vmConn.Close()
	}

	if n.vmFile != nil {
		_ = n.vmFile.Close()
	}

	for _, s := range n.services {
		_ = s.Close()
	}

	// Aborting every stack endpoint unblocks workers that read from the guest, such as an idle DNS TCP connection.
	if n.stack != nil {
		n.stack.Close()
	}

	var errs []error
	if !waitTimeout(n.wg.Wait, closeTimeout) {
		errs = append(errs, fmt.Errorf("network close: flows still running after %v", closeTimeout))
	}

	if n.stack != nil && !waitTimeout(n.stack.Destroy, closeTimeout) {
		errs = append(errs, fmt.Errorf("network close: stack still running after %v", closeTimeout))
	}

	return errors.Join(errs...)
}

// enter registers one unit of work that Close waits for. It reports false after Close.
// The caller must call n.wg.Done when the work ends.
func (n *Network) enter() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed {
		return false
	}

	n.wg.Add(1)

	return true
}

// waitTimeout runs fn and reports whether it returned within d. After a timeout fn keeps running in the background.
func waitTimeout(fn func(), d time.Duration) bool {
	done := make(chan struct{})

	go func() {
		fn()
		close(done)
	}()

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func normalizePublish(in []Publish) ([]Publish, error) {
	out := make([]Publish, 0, len(in))

	for _, p := range in {
		addr := p.Host.Addr()
		if !addr.IsValid() {
			addr = loopback
		}

		p.Host = netip.AddrPortFrom(addr, p.Host.Port())

		if !addr.Is4() {
			return nil, fmt.Errorf("publish %s: host address must be IPv4", p)
		}

		if p.Guest == 0 {
			return nil, fmt.Errorf("publish %s: guest port must not be 0", p)
		}

		out = append(out, p)
	}

	return out, nil
}

func newPolicy(allowLAN bool, publish []Publish) *policy {
	ports := make(map[uint16]bool, len(publish))
	for _, p := range publish {
		ports[p.Guest] = true
	}

	return &policy{
		allowLAN:  allowLAN,
		subnet:    subnet,
		gateway:   gatewayIP,
		guestIP:   guestIP,
		guestMAC:  guestHWAddr,
		published: ports,
		hostAddrs: atomic.Pointer[map[netip.Addr]bool]{},
	}
}

func mustParseMAC(s string) net.HardwareAddr {
	mac, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}

	return mac
}

func tcpAddr(a netip.Addr) tcpip.Address { return tcpip.AddrFrom4(a.As4()) }
