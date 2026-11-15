package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
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

		c, err := nd.DialContext(ctx, "udp4", pc.LocalAddr().String())
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
	n, g := startNetwork(t, Config{MaxTCP: 1, Dial: d.dial, Hosts: map[string]netip.Addr{testHost: publicAddr}})

	// The idle DNS connection holds the only TCP slot until the DNS server closes it.
	queryOverTCP(t, g)

	// DNS connections past the cap get a reset and leave no endpoint behind.
	endpoints := len(n.stack.RegisteredEndpoints())

	for range 5 {
		start := time.Now()

		if _, err := dialTCP(t, g, netip.AddrPortFrom(gatewayIP, 53).String(), 5*time.Second); err == nil {
			t.Fatal("DNS TCP connection past the cap works")
		}

		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("DNS TCP connection past the cap failed after %v, want a reset", elapsed)
		}
	}

	if got := len(n.stack.RegisteredEndpoints()); got != endpoints {
		t.Errorf("stack endpoints = %d after rejected connections, want %d", got, endpoints)
	}

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

// fakeNameserver serves UDP and TCP on one local port and records the queries it gets.
// It answers TXT queries with the transport name. Over UDP, big.test gets a truncated answer.
// Over TCP, tcpfail.test gets no answer.
func fakeNameserver(t *testing.T) (string, func() []*dns.Msg) {
	t.Helper()

	var (
		mu  sync.Mutex
		got []*dns.Msg
	)

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		mu.Lock()

		got = append(got, r.Copy())

		mu.Unlock()

		network := w.LocalAddr().Network()
		overTCP := network == "tcp"
		name := r.Question[0].Name

		if overTCP && name == "tcpfail.test." {
			_ = w.Close()
			return
		}

		m := new(dns.Msg)
		m.SetReply(r)

		if !overTCP && (name == "big.test." || name == "tcpfail.test.") {
			m.Truncated = true
		} else {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{network}})
		}

		_ = w.WriteMsg(m)
	})

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := lc.Listen(t.Context(), "tcp4", pc.LocalAddr().String())
	if err != nil {
		_ = pc.Close()

		t.Skipf("TCP port of the fake nameserver is taken: %v", err)
	}

	for _, srv := range []*dns.Server{{PacketConn: pc, Handler: handler}, {Listener: ln, Handler: handler}} {
		go func() { _ = srv.ActivateAndServe() }()

		t.Cleanup(func() { _ = srv.Shutdown() })
	}

	return pc.LocalAddr().String(), func() []*dns.Msg {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(got)
	}
}

func TestDNSForward(t *testing.T) {
	t.Parallel()

	ns, received := fakeNameserver(t)

	query := func(name string, qtype uint16) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(name, qtype)

		return m
	}

	notify := new(dns.Msg)
	notify.SetNotify("notify.test.")

	chaos := query("version.bind.", dns.TypeTXT)
	chaos.Question[0].Qclass = dns.ClassCHAOS

	two := query("two.test.", dns.TypeTXT)
	two.Question = append(two.Question, two.Question[0])

	guestFlags := query("small.test.", dns.TypeTXT)
	guestFlags.RecursionDesired = false
	guestFlags.SetEdns0(1232, true)
	guestFlags.IsEdns0().Option = append(guestFlags.IsEdns0().Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"})

	tests := []struct {
		name      string
		msg       *dns.Msg
		rcode     int
		answer    string
		truncated bool
		forwarded bool
	}{
		{name: "notify", msg: notify, rcode: dns.RcodeNotImplemented},
		{name: "chaos", msg: chaos, rcode: dns.RcodeRefused},
		{name: "axfr", msg: query("zone.test.", dns.TypeAXFR), rcode: dns.RcodeNotImplemented},
		{name: "two questions", msg: two, rcode: dns.RcodeFormatError},
		{name: "rebuilt query", msg: guestFlags, rcode: dns.RcodeSuccess, answer: "udp", forwarded: true},
		{name: "truncated udp then tcp", msg: query("big.test.", dns.TypeTXT), rcode: dns.RcodeSuccess, answer: "tcp", forwarded: true},
		{name: "truncated after tcp fails", msg: query("tcpfail.test.", dns.TypeTXT), rcode: dns.RcodeSuccess, truncated: true, forwarded: true},
	}

	h := newDNSHandler(t.Context(), nil, nil, []string{ns})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := h.answer(t.Context(), tt.msg)

			var answer string

			if len(m.Answer) == 1 {
				if txt, ok := m.Answer[0].(*dns.TXT); ok {
					answer = strings.Join(txt.Txt, "")
				}
			}

			if m.Rcode != tt.rcode || answer != tt.answer || m.Truncated != tt.truncated {
				t.Errorf("reply rcode %s, answer %q, truncated %v, want %s, %q, %v",
					dns.RcodeToString[m.Rcode], answer, m.Truncated, dns.RcodeToString[tt.rcode], tt.answer, tt.truncated)
			}

			var forwarded []*dns.Msg

			for _, r := range received() {
				if r.Question[0].Name == tt.msg.Question[0].Name {
					forwarded = append(forwarded, r)
				}
			}

			if (len(forwarded) > 0) != tt.forwarded {
				t.Fatalf("nameserver got %d queries, want forwarded %v", len(forwarded), tt.forwarded)
			}

			for _, r := range forwarded {
				opt := r.IsEdns0()
				if !r.RecursionDesired || len(r.Question) != 1 || r.Question[0].Qclass != dns.ClassINET || opt == nil || len(opt.Option) != 0 || opt.Do() {
					t.Errorf("forwarded query keeps guest flags or options:\n%v", r)
				}
			}
		})
	}
}

func TestDNSForwardWithoutNameserver(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	// A closed port refuses the query at once.
	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := pc.LocalAddr().String()
	_ = pc.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeTXT)

	if m := newDNSHandler(t.Context(), nil, nil, []string{addr}).answer(t.Context(), q); m.Rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
}

func TestDNSForwardSkipsDeadNameserver(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	// A UDP socket that never answers holds the first nameserver until its share of the budget ends.
	dead, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dead.Close() })

	live, _ := fakeNameserver(t)

	q := new(dns.Msg)
	q.SetQuestion("example.org.", dns.TypeTXT)

	m := newDNSHandler(t.Context(), nil, nil, []string{dead.LocalAddr().String(), live}).answer(t.Context(), q)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) == 0 {
		t.Errorf("rcode = %s with %d answers, want an answer from the second nameserver", dns.RcodeToString[m.Rcode], len(m.Answer))
	}
}

func TestDNSReplyFitsTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		udp       bool
		edns      bool
		truncated bool
	}{
		{name: "udp without edns", udp: true, edns: false, truncated: true},
		{name: "udp with edns", udp: true, edns: true, truncated: false},
		{name: "tcp", udp: false, edns: false, truncated: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := new(dns.Msg)
			r.SetQuestion("big.test.", dns.TypeTXT)

			if tt.edns {
				r.SetEdns0(4096, false)
			}

			m := new(dns.Msg)
			m.SetReply(r)

			// About 1000 bytes: above 512, below 4096.
			for range 4 {
				m.Answer = append(m.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: "big.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET},
					Txt: []string{strings.Repeat("x", 250)},
				})
			}

			fit(m, r, tt.udp)

			if m.Truncated != tt.truncated {
				t.Errorf("truncated = %v, want %v", m.Truncated, tt.truncated)
			}

			if got := m.IsEdns0() != nil; got != tt.edns {
				t.Errorf("reply has OPT = %v, want %v", got, tt.edns)
			}
		})
	}
}
