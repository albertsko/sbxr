package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/netstack/guestsim"
	"github.com/miekg/dns"
)

// testHost is a name that the tests map to publicAddr through Config.Hosts.
const testHost = "example.test"

// publicAddr is a documentation address (TEST-NET-3). The egress policy treats it as public.
var publicAddr = netip.MustParseAddr("203.0.113.10")

// testDialer is the Dial hook of the tests, so no test needs the internet.
// It records every dial that passed the egress check, sends dials to routed addresses to a local server, and dials all other addresses directly.
// A test can route a blocked address to a working server, so a policy bug shows up as a working connection.
type testDialer struct {
	tcp, udp string // Local servers for routed addresses.
	routed   []netip.Addr

	mu    sync.Mutex
	dials []string
}

func (d *testDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, network+" "+address)
	d.mu.Unlock()

	if ap, err := netip.ParseAddrPort(address); err == nil && slices.Contains(d.routed, ap.Addr()) {
		address = d.tcp
		if network == "udp" {
			address = d.udp
		}
	}

	var nd net.Dialer

	c, err := nd.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("test dial: %w", err)
	}

	return c, nil
}

func (d *testDialer) Dials() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.dials)
}

func (d *testDialer) dialed(addr netip.Addr) bool {
	return slices.ContainsFunc(d.Dials(), func(s string) bool { return strings.Contains(s, " "+addr.String()+":") })
}

// newTestDialer starts local TCP and UDP echo servers and routes addrs to them.
func newTestDialer(t *testing.T, addrs ...netip.Addr) *testDialer {
	t.Helper()

	return &testDialer{tcp: tcpEcho(t, "127.0.0.1:0"), udp: udpEcho(t), routed: append(addrs, publicAddr)}
}

func startNetwork(t *testing.T, cfg Config) (*Network, *guestsim.Guest) {
	t.Helper()

	n, err := Start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	g, err := guestsim.Start(ctx, n.VMFile(), n.GuestMAC())
	if err != nil {
		_ = n.Close()

		t.Fatalf("guestsim.Start: %v", err)
	}

	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Errorf("guest Close: %v", err)
		}

		if err := n.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return n, g
}

// tcpEcho starts a TCP echo server and returns its address.
func tcpEcho(t *testing.T, addr string) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go serve(ln, func(c net.Conn) { _, _ = io.Copy(c, c) })

	return ln.Addr().String()
}

// serve calls handle for each connection of ln in its own goroutine and closes the connection after it.
func serve(ln net.Listener, handle func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}

		go func() {
			defer c.Close()

			handle(c)
		}()
	}
}

// udpEcho starts a UDP echo server on 127.0.0.1 and returns its address.
func udpEcho(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 2048)

		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}

			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()

	return pc.LocalAddr().String()
}

// roundTrip writes msg to c and reports whether the same bytes come back before timeout.
func roundTrip(c net.Conn, msg string, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))

	if _, err := c.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	if string(buf) != msg {
		return errors.New("echo mismatch: " + string(buf))
	}

	return nil
}

func dialTCP(t *testing.T, g *guestsim.Guest, addr string, timeout time.Duration) (net.Conn, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	c, err := g.DialTCP(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("guest dial: %w", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c, nil
}

func dialUDP(t *testing.T, g *guestsim.Guest, addr string) net.Conn {
	t.Helper()

	c, err := g.DialUDP(t.Context(), addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestGuestGetsLeaseAndReachesPublicAddresses(t *testing.T) {
	t.Parallel()

	d := newTestDialer(t)
	n, g := startNetwork(t, Config{Dial: d.dial, Hosts: map[string]netip.Addr{testHost: publicAddr}})

	if g.Addr() != n.GuestIP() {
		t.Errorf("guest address = %v, want %v", g.Addr(), n.GuestIP())
	}

	t.Run("dns", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		got, err := g.LookupHost(ctx, "Example.Test")
		if err != nil || len(got) != 1 || got[0] != publicAddr {
			t.Errorf("LookupHost = %v, %v, want [%v]", got, err, publicAddr)
		}

		// Other types for a name in Hosts get an empty answer without the host resolver.
		c := dialUDP(t, g, netip.AddrPortFrom(gatewayIP, 53).String())

		q := new(dns.Msg)
		q.SetQuestion("example.test.", dns.TypeAAAA)

		conn := &dns.Conn{Conn: c}
		if err := conn.WriteMsg(q); err != nil {
			t.Fatal(err)
		}

		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

		r, err := conn.ReadMsg()
		if err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Errorf("AAAA answer = %v, %v, want empty success", r, err)
		}
	})

	t.Run("tcp by name", func(t *testing.T) {
		t.Parallel()

		c, err := dialTCP(t, g, "example.test:80", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		if err := roundTrip(c, "hello tcp", 5*time.Second); err != nil {
			t.Error(err)
		}
	})

	t.Run("udp", func(t *testing.T) {
		t.Parallel()

		c := dialUDP(t, g, netip.AddrPortFrom(publicAddr, 53).String())
		if err := roundTrip(c, "hello udp", 5*time.Second); err != nil {
			t.Error(err)
		}
	})
}

// firstHostIPv4 returns a non-loopback IPv4 address of a host interface.
func firstHostIPv4() (netip.Addr, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, false
	}

	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err == nil && pfx.Addr().Is4() && !pfx.Addr().IsLoopback() && !pfx.Addr().IsLinkLocalUnicast() {
			return pfx.Addr(), true
		}
	}

	return netip.Addr{}, false
}

// hostIPv4 returns a non-loopback IPv4 address of a host interface, or skips the test.
func hostIPv4(t *testing.T) netip.Addr {
	t.Helper()

	addr, ok := firstHostIPv4()
	if !ok {
		t.Skip("no non-loopback IPv4 host address")
	}

	return addr
}

// hostCase in egressCases stands for a non-loopback address of a host interface.
const hostCase = "host"

// egressCases are guest destinations that the default policy blocks. lan reports whether AllowLAN opens them.
var egressCases = []struct {
	addr string
	lan  bool
}{
	{addr: "127.0.0.1"},
	{addr: "0.0.0.0"},
	{addr: "169.254.169.254"},
	{addr: "10.0.0.1", lan: true},
	{addr: "172.16.0.1", lan: true},
	{addr: "192.168.1.1", lan: true},
	{addr: "100.64.0.1", lan: true},
	{addr: "192.168.127.1"},
	{addr: hostCase, lan: true},
}

func TestEgressPolicy(t *testing.T) {
	t.Parallel()

	for _, tt := range egressCases {
		for _, allowLAN := range []bool{false, true} {
			name := tt.addr
			if allowLAN {
				name += " allow-lan"
			}

			want := allowLAN && tt.lan

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var addr netip.Addr
				if tt.addr == hostCase {
					addr = hostIPv4(t)
				} else {
					addr = netip.MustParseAddr(tt.addr)
				}

				d := newTestDialer(t, addr)
				_, g := startNetwork(t, Config{AllowLAN: allowLAN, Dial: d.dial})

				dst := netip.AddrPortFrom(addr, 9)

				// A blocked TCP connection gets a reset from the frame filter, so it fails long before the dial timeout.
				start := time.Now()

				c, err := dialTCP(t, g, dst.String(), 5*time.Second)
				if err == nil {
					err = roundTrip(c, "tcp", time.Second)
				}

				if got := err == nil; got != want {
					t.Errorf("tcp works = %v (%v), want %v", got, err, want)
				}

				// The guest stack never sends to loopback or unspecified addresses, so those dials time out in the guest.
				local := addr.IsLoopback() || addr.IsUnspecified()
				if elapsed := time.Since(start); !want && !local && elapsed > 2*time.Second {
					t.Errorf("blocked tcp failed after %v, want a reset", elapsed)
				}

				u := dialUDP(t, g, dst.String())
				if err := roundTrip(u, "udp", 500*time.Millisecond); (err == nil) != want {
					t.Errorf("udp works = %v (%v), want %v", err == nil, err, want)
				}

				if got := d.dialed(addr); got != want {
					t.Errorf("host dial attempted = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestAllowLANReachesHostAddress(t *testing.T) {
	t.Parallel()

	host := hostIPv4(t)
	srv := tcpEcho(t, netip.AddrPortFrom(host, 0).String())
	_, g := startNetwork(t, Config{AllowLAN: true})

	c, err := dialTCP(t, g, srv, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if err := roundTrip(c, "lan", 5*time.Second); err != nil {
		t.Error(err)
	}
}
