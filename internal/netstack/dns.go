package netstack

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// dnsTimeout bounds each guest query. Close cancels host resolver lookups, and forwarded queries end within this timeout.
	dnsTimeout = 2 * time.Second

	// maxDNSQueries bounds the queries that the gateway resolves at once. Further queries are dropped, and guest resolvers retry them.
	maxDNSQueries = 64

	resolvConf = "/etc/resolv.conf"
)

// dnsHandler answers guest queries on the gateway.
// A queries go to the host resolver, AAAA queries get no answer because the guest has no IPv6 route, and other types are forwarded to the host's nameservers.
type dnsHandler struct {
	ctx         context.Context //nolint:containedctx // Close cancels host resolver lookups through it.
	hosts       map[string]netip.Addr
	resolver    *net.Resolver
	nameservers []string
	udp         *dns.Client
	tcp         *dns.Client
	slots       chan struct{}
}

// newDNSHandler reads the nameservers from /etc/resolv.conf when nameservers is empty.
func newDNSHandler(ctx context.Context, hosts map[string]netip.Addr, resolver *net.Resolver, nameservers []string) *dnsHandler {
	if len(nameservers) == 0 {
		if conf, err := dns.ClientConfigFromFile(resolvConf); err == nil {
			for _, s := range conf.Servers {
				nameservers = append(nameservers, net.JoinHostPort(s, conf.Port))
			}
		}
	}

	return &dnsHandler{
		ctx:         ctx,
		hosts:       hosts,
		resolver:    cmp.Or(resolver, net.DefaultResolver),
		nameservers: nameservers,
		udp:         &dns.Client{Net: "udp", Timeout: dnsTimeout},
		tcp:         &dns.Client{Net: "tcp", Timeout: dnsTimeout},
		slots:       make(chan struct{}, maxDNSQueries),
	}
}

func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	select {
	case h.slots <- struct{}{}:
	default:
		return
	}

	defer func() { <-h.slots }()

	ctx, cancel := context.WithTimeout(h.ctx, dnsTimeout)
	defer cancel()

	m := h.answer(ctx, r)

	size := dns.MaxMsgSize
	if w.LocalAddr().Network() == "udp" {
		size = dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil {
			size = int(opt.UDPSize())
		}
	}

	m.Truncate(size)
	_ = w.WriteMsg(m)
}

func (h *dnsHandler) answer(ctx context.Context, r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true

	switch {
	case r.Opcode != dns.OpcodeQuery:
		m.Rcode = dns.RcodeNotImplemented
		return m
	case len(r.Question) != 1:
		m.Rcode = dns.RcodeFormatError
		return m
	}

	q := r.Question[0]
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")

	switch {
	case q.Qclass != dns.ClassINET:
		m.Rcode = dns.RcodeRefused
		return m
	case q.Qtype == dns.TypeAXFR || q.Qtype == dns.TypeIXFR:
		m.Rcode = dns.RcodeNotImplemented
		return m
	}

	// Names in hosts never reach the host resolver. Types other than A get an empty answer.
	if a, ok := h.hosts[name]; ok {
		if q.Qtype == dns.TypeA {
			m.Answer = append(m.Answer, aRecord(q.Name, a))
		}

		return m
	}

	switch q.Qtype {
	case dns.TypeA:
		addrs, err := h.resolver.LookupNetIP(ctx, "ip4", name)
		if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
			m.Rcode = dns.RcodeNameError
			return m
		}

		if err != nil {
			m.Rcode = dns.RcodeServerFailure
			return m
		}

		for _, a := range addrs {
			m.Answer = append(m.Answer, aRecord(q.Name, a.Unmap()))
		}
	case dns.TypeAAAA:
	default:
		h.forward(ctx, q, m)
	}

	return m
}

// forward asks the host's nameservers, because the Go resolver has no generic query.
// The query is rebuilt, so guest flags and options never reach the nameservers.
// A truncated UDP answer is asked again over TCP. If that fails, the reply stays truncated, so the guest retries over TCP.
func (h *dnsHandler) forward(ctx context.Context, q dns.Question, m *dns.Msg) {
	req := new(dns.Msg)
	req.SetQuestion(q.Name, q.Qtype)
	req.SetEdns0(dns.DefaultMsgSize, false)

	for _, ns := range h.nameservers {
		resp, _, err := h.udp.ExchangeContext(ctx, req, ns)
		if err != nil {
			continue
		}

		if resp.Truncated {
			if full, _, err := h.tcp.ExchangeContext(ctx, req, ns); err == nil {
				resp = full
			}
		}

		m.Answer = resp.Answer
		m.Ns = resp.Ns
		m.Rcode = resp.Rcode
		m.Truncated = resp.Truncated

		return
	}

	m.Rcode = dns.RcodeServerFailure
}

func aRecord(name string, a netip.Addr) *dns.A {
	return &dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET},
		A:   a.AsSlice(),
	}
}

// acceptDNS hands a guest DNS TCP connection to the DNS server.
// The connection holds a TCP cap slot until it closes. Past the cap, the SYN gets a reset before any endpoint exists.
func (n *Network) acceptDNS(r *tcp.ForwarderRequest) {
	if !n.enter() {
		r.Complete(true)
		return
	}
	defer n.wg.Done()

	if !n.tcp.acquire() {
		n.logger.Debug("netstack: tcp cap reached", "dst", "dns")
		r.Complete(true)

		return
	}

	var wq waiter.Queue

	ep, tcpErr := r.CreateEndpoint(&wq)
	r.Complete(false)

	if tcpErr != nil {
		n.tcp.release()
		return
	}

	c := &releaseConn{Conn: gonet.NewTCPConn(&wq, ep), release: sync.OnceFunc(n.tcp.release)}

	select {
	case n.dnsConns <- c:
	case <-n.ctx.Done():
		_ = c.Close()
	}
}

// connListener is the DNS TCP listener. acceptDNS feeds it from the TCP forwarder.
type connListener struct {
	conns <-chan net.Conn
	done  <-chan struct{}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (*connListener) Close() error { return nil }

func (*connListener) Addr() net.Addr {
	return &net.TCPAddr{IP: gatewayIP.AsSlice(), Port: portDNS, Zone: ""}
}

// releaseConn releases its cap slot when it closes.
type releaseConn struct {
	net.Conn

	release func()
}

func (c *releaseConn) Close() error {
	defer c.release()

	return c.Conn.Close() //nolint:wrapcheck // The DNS server ignores the error.
}
