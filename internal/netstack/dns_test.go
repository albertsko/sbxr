package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/netstack/guestsim"
	"github.com/miekg/dns"
)

// blackholeResolver returns a host resolver whose queries go to a UDP server that never answers.
// It reports the most host connections that were open at once.
func blackholeResolver(t *testing.T) (*net.Resolver, func() int64) {
	t.Helper()

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = pc.Close() })

	var open, peak atomic.Int64

	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var nd net.Dialer

		c, err := nd.DialContext(ctx, "udp", pc.LocalAddr().String())
		if err != nil {
			return nil, fmt.Errorf("blackhole dial: %w", err)
		}

		cur := open.Add(1)

		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}

		return &countedConn{Conn: c, close: sync.OnceFunc(func() { open.Add(-1) })}, nil
	}}

	return r, peak.Load
}

type countedConn struct {
	net.Conn

	close func()
}

func (c *countedConn) Close() error {
	c.close()

	return c.Conn.Close() //nolint:wrapcheck // Test helper.
}

func TestDNSQueryFloodIsBounded(t *testing.T) {
	t.Parallel()

	resolver, peak := blackholeResolver(t)
	_, g := startNetwork(t, Config{resolver: resolver, Hosts: map[string]netip.Addr{testHost: publicAddr}})

	c := dialUDP(t, g, netip.AddrPortFrom(gatewayIP, 53).String())
	conn := &dns.Conn{Conn: c}

	for i := range 1000 {
		q := new(dns.Msg)
		q.SetQuestion(fmt.Sprintf("n%d.flood.test.", i), dns.TypeA)

		if err := conn.WriteMsg(q); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	if got := peak(); got == 0 || got > maxDNSQueries {
		t.Errorf("peak host resolver connections = %d, want 1 to %d", got, maxDNSQueries)
	}

	// The slots come back after the query timeout.
	err := eventually(10*time.Second, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		if _, err := g.LookupHost(ctx, testHost); err != nil {
			return fmt.Errorf("lookup: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Errorf("DNS after the flood: %v", err)
	}
}

func TestDNSTCPCountsAgainstCap(t *testing.T) {
	t.Parallel()

	d := newTestDialer(t)
	_, g := startNetwork(t, Config{MaxTCP: 1, Dial: d.dial, Hosts: map[string]netip.Addr{testHost: publicAddr}})

	// The idle DNS connection holds the only TCP slot until the DNS server closes it.
	queryOverTCP(t, g)

	dst := netip.AddrPortFrom(publicAddr, 80).String()
	if c, err := dialTCP(t, g, dst, 5*time.Second); err == nil && roundTrip(c, "over", time.Second) == nil {
		t.Fatal("TCP connection works while DNS holds the cap")
	}

	err := eventually(2*dnsTimeout, func() error {
		c, err := dialTCP(t, g, dst, time.Second)
		if err != nil {
			return err
		}

		return roundTrip(c, "after", time.Second)
	})
	if err != nil {
		t.Errorf("TCP connection after the DNS connection idled out: %v", err)
	}
}

func TestCloseWithIdleDNSConnection(t *testing.T) {
	t.Parallel()

	n, g := startNetwork(t, Config{Hosts: map[string]netip.Addr{testHost: publicAddr}})

	queryOverTCP(t, g)
	assertQuickClose(t, n)
}

func TestCloseWithUnansweredSYN(t *testing.T) {
	t.Parallel()

	n, err := Start(t.Context(), Config{Publish: []Publish{{Guest: 80}}, Hosts: map[string]netip.Addr{testHost: publicAddr}})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// The gateway resolves the guest MAC through DHCP and DNS, then the guest disappears.
	g, err := guestsim.Start(ctx, n.VMFile(), n.GuestMAC())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.LookupHost(ctx, testHost); err != nil {
		t.Fatal(err)
	}

	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	var nd net.Dialer

	host, err := nd.DialContext(t.Context(), "tcp", n.Published()[0].Host.String())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	// The gateway now sends a SYN that nobody answers.
	time.Sleep(200 * time.Millisecond)

	assertQuickClose(t, n)
}

func assertQuickClose(t *testing.T, n *Network) {
	t.Helper()

	start := time.Now()

	if err := n.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v", elapsed)
	}
}

// queryOverTCP sends one query for testHost over a new DNS TCP connection and leaves the connection open.
// The answer shows that the DNS server has accepted the connection.
func queryOverTCP(t *testing.T, g *guestsim.Guest) {
	t.Helper()

	c, err := dialTCP(t, g, netip.AddrPortFrom(gatewayIP, 53).String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	conn := &dns.Conn{Conn: c}

	q := new(dns.Msg)
	q.SetQuestion(testHost+".", dns.TypeA)

	if err := conn.WriteMsg(q); err != nil {
		t.Fatal(err)
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.ReadMsg(); err != nil {
		t.Fatal(err)
	}
}
