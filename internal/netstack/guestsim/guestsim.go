// Package guestsim simulates the network of a VM guest on the VM end of the netstack socket.
// Tests and the fake hypervisor use it to get an address with DHCP, resolve names through the gateway DNS, connect out, and listen.
package guestsim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID        = 1
	mtu          = 1500
	queueLen     = 256
	maxFrameSize = 65535
	dhcpTimeout  = 5 * time.Second
)

// Guest is a simulated guest network stack.
type Guest struct {
	stack    *stack.Stack
	link     *channel.Endpoint
	conn     net.Conn
	addr     netip.Addr
	gateway  netip.Addr
	resolver *net.Resolver

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start attaches a guest with the MAC address mac to the socket f and configures it with DHCP.
// Start duplicates f, so the caller still owns f.
func Start(ctx context.Context, f *os.File, mac net.HardwareAddr) (*Guest, error) {
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("guest socket: %w", err)
	}

	link := channel.New(queueLen, mtu+header.EthernetMinimumSize, tcpip.LinkAddress(mac))
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	pumpCtx, cancel := context.WithCancel(context.Background())
	g := &Guest{stack: s, link: link, conn: conn, cancel: cancel}

	if err := s.CreateNIC(nicID, ethernet.New(link)); err != nil {
		_ = g.Close()
		return nil, fmt.Errorf("guest interface: %s", err)
	}

	g.wg.Go(func() { g.sendFrames(pumpCtx) })
	g.wg.Go(g.receiveFrames)

	if err := g.configure(ctx, mac); err != nil {
		_ = g.Close()
		return nil, err
	}

	g.resolver = &net.Resolver{PreferGo: true, Dial: g.dialDNS}

	return g, nil
}

// Addr returns the address that DHCP gave the guest.
func (g *Guest) Addr() netip.Addr { return g.addr }

// DialTCP connects to address, a host and port. A host name is resolved through the gateway DNS.
func (g *Guest) DialTCP(ctx context.Context, address string) (net.Conn, error) {
	dst, err := g.resolve(ctx, address)
	if err != nil {
		return nil, err
	}

	c, err := gonet.DialContextTCP(ctx, g.stack, dst, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("guest dial %s: %w", address, err)
	}

	return c, nil
}

// DialUDP returns a UDP socket connected to address, a host and port. A host name is resolved through the gateway DNS.
func (g *Guest) DialUDP(ctx context.Context, address string) (net.Conn, error) {
	dst, err := g.resolve(ctx, address)
	if err != nil {
		return nil, err
	}

	c, err := gonet.DialUDP(g.stack, nil, &dst, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("guest dial %s: %w", address, err)
	}

	return c, nil
}

// ListenTCP listens on the guest address and port.
func (g *Guest) ListenTCP(port uint16) (net.Listener, error) {
	ln, err := gonet.ListenTCP(g.stack, g.fullAddr(g.addr, port), ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("guest listen %d: %w", port, err)
	}

	return ln, nil
}

// LookupHost resolves name to IPv4 addresses through the gateway DNS.
func (g *Guest) LookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	addrs, err := g.resolver.LookupNetIP(ctx, "ip4", name)
	if err != nil {
		return nil, fmt.Errorf("guest lookup: %w", err)
	}

	return addrs, nil
}

// Close stops the guest stack and closes its socket.
func (g *Guest) Close() error {
	g.cancel()
	err := g.conn.Close()
	g.link.Close()
	g.wg.Wait()
	g.stack.Destroy()

	if err != nil {
		return fmt.Errorf("guest close: %w", err)
	}

	return nil
}

func (g *Guest) sendFrames(ctx context.Context) {
	for {
		pkt := g.link.ReadContext(ctx)
		if pkt == nil {
			return
		}

		v := pkt.ToView()
		err := g.write(v.AsSlice())

		v.Release()
		pkt.DecRef()

		if err != nil {
			return
		}
	}
}

// write sends one frame. A full peer queue returns ENOBUFS on macOS, so the write is retried like a NIC that waits for its queue.
func (g *Guest) write(frame []byte) error {
	for {
		_, err := g.conn.Write(frame)
		if !errors.Is(err, syscall.ENOBUFS) {
			return err //nolint:wrapcheck // The error only ends the frame pump.
		}

		time.Sleep(time.Millisecond)
	}
}

func (g *Guest) receiveFrames() {
	buf := make([]byte, maxFrameSize)

	for {
		n, err := g.conn.Read(buf)
		if err != nil {
			return
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(buf[:n])})
		g.link.InjectInbound(0, pkt)
		pkt.DecRef()
	}
}

// configure runs DHCP from the unspecified address, then sets the leased address, the routes, and the DNS server.
func (g *Guest) configure(ctx context.Context, mac net.HardwareAddr) error {
	anyAddr := tcpip.AddrFrom4([4]byte{})
	if err := g.addAddress(anyAddr, 0); err != nil {
		return err
	}

	g.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	ack, err := g.dhcp(ctx, mac)
	if err != nil {
		return err
	}

	if err := g.stack.RemoveAddress(nicID, anyAddr); err != nil {
		return fmt.Errorf("guest address: %s", err)
	}

	addr, ok := netip.AddrFromSlice(ack.YourIPAddr.To4())
	routers := ack.Router()

	if !ok || len(routers) == 0 {
		return errors.New("guest dhcp: lease has no address or router")
	}

	ones, _ := ack.SubnetMask().Size()
	if err := g.addAddress(tcpip.AddrFrom4(addr.As4()), ones); err != nil {
		return err
	}

	g.addr = addr
	g.gateway, _ = netip.AddrFromSlice(routers[0].To4())

	pfx := netip.PrefixFrom(addr, ones).Masked()

	local, err := tcpip.NewSubnet(tcpip.AddrFrom4(pfx.Addr().As4()), tcpip.MaskFromBytes(ack.SubnetMask()))
	if err != nil {
		return fmt.Errorf("guest route: %w", err)
	}

	g.stack.SetRouteTable([]tcpip.Route{
		{Destination: local, NIC: nicID},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(g.gateway.As4()), NIC: nicID},
	})

	return nil
}

func (g *Guest) addAddress(a tcpip.Address, prefixLen int) error {
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: a, PrefixLen: prefixLen},
	}
	if err := g.stack.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("guest address: %s", err)
	}

	return nil
}

// dhcp runs one DISCOVER, OFFER, REQUEST, ACK exchange.
func (g *Guest) dhcp(ctx context.Context, mac net.HardwareAddr) (*dhcpv4.DHCPv4, error) {
	var wq waiter.Queue

	ep, tcpErr := g.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if tcpErr != nil {
		return nil, fmt.Errorf("guest dhcp: %s", tcpErr)
	}

	ep.SocketOptions().SetBroadcast(true)

	if tcpErr := ep.Bind(tcpip.FullAddress{NIC: nicID, Port: dhcpv4.ClientPort}); tcpErr != nil {
		ep.Close()
		return nil, fmt.Errorf("guest dhcp: %s", tcpErr)
	}

	conn := gonet.NewUDPConn(&wq, ep)
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(dhcpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	_ = conn.SetDeadline(deadline)

	discover, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		return nil, fmt.Errorf("guest dhcp: %w", err)
	}

	offer, err := exchange(conn, discover, dhcpv4.MessageTypeOffer)
	if err != nil {
		return nil, err
	}

	request, err := dhcpv4.NewRequestFromOffer(offer)
	if err != nil {
		return nil, fmt.Errorf("guest dhcp: %w", err)
	}

	return exchange(conn, request, dhcpv4.MessageTypeAck)
}

func exchange(conn *gonet.UDPConn, msg *dhcpv4.DHCPv4, want dhcpv4.MessageType) (*dhcpv4.DHCPv4, error) {
	server := &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ServerPort}
	if _, err := conn.WriteTo(msg.ToBytes(), server); err != nil {
		return nil, fmt.Errorf("guest dhcp: %w", err)
	}

	buf := make([]byte, maxFrameSize)

	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return nil, fmt.Errorf("guest dhcp: waiting for %s: %w", want, err)
		}

		reply, err := dhcpv4.FromBytes(buf[:n])
		if err == nil && reply.TransactionID == msg.TransactionID && reply.MessageType() == want {
			return reply, nil
		}
	}
}

func (g *Guest) resolve(ctx context.Context, address string) (tcpip.FullAddress, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("guest address %q: %w", address, err)
	}

	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return tcpip.FullAddress{}, fmt.Errorf("guest address %q: %w", address, err)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		addrs, err := g.LookupHost(ctx, host)
		if err != nil {
			return tcpip.FullAddress{}, err
		}

		addr = addrs[0]
	}

	return g.fullAddr(addr, uint16(port)), nil
}

func (*Guest) fullAddr(a netip.Addr, port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFromSlice(a.AsSlice()), Port: port}
}

// dialDNS sends the queries of the Go resolver to the gateway DNS, whatever server the host configuration names.
func (g *Guest) dialDNS(ctx context.Context, network, _ string) (net.Conn, error) {
	server := net.JoinHostPort(g.gateway.String(), "53")
	if network == "tcp" || network == "tcp4" {
		return g.DialTCP(ctx, server)
	}

	return g.DialUDP(ctx, server)
}
