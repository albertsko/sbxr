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
)

const (
	// dnsTimeout bounds each guest query, so Close never waits for the host resolver.
	dnsTimeout = 2 * time.Second

	// maxDNSQueries bounds the queries that the gateway resolves at once. Further queries are dropped, and guest resolvers retry them.
	maxDNSQueries = 64

	resolvConf = "/etc/resolv.conf"
)

// dnsHandler answers guest queries on the gateway.
// A queries go to the host resolver, AAAA queries get no answer because the guest has no IPv6 route, and other types are forwarded to the host's nameservers.
type dnsHandler struct {
	ctx         context.Context //nolint:containedctx // Close cancels running queries through it.
	hosts       map[string]netip.Addr
	resolver    *net.Resolver
	nameservers []string
	client      *dns.Client
	slots       chan struct{}
}

func newDNSHandler(ctx context.Context, hosts map[string]netip.Addr, resolver *net.Resolver) *dnsHandler {
	var nameservers []string

	if conf, err := dns.ClientConfigFromFile(resolvConf); err == nil {
		for _, s := range conf.Servers {
			nameservers = append(nameservers, net.JoinHostPort(s, conf.Port))
		}
	}

	return &dnsHandler{
		ctx:         ctx,
		hosts:       hosts,
		resolver:    cmp.Or(resolver, net.DefaultResolver),
		nameservers: nameservers,
		client:      &dns.Client{Timeout: dnsTimeout},
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

	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		return m
	}

	q := r.Question[0]
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")

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
		h.forward(ctx, r, m)
	}

	return m
}

// forward asks the host's nameservers, because the Go resolver has no generic query.
func (h *dnsHandler) forward(ctx context.Context, r, m *dns.Msg) {
	for _, ns := range h.nameservers {
		resp, _, err := h.client.ExchangeContext(ctx, r, ns)
		if err != nil {
			continue
		}

		m.Answer = resp.Answer
		m.Ns = resp.Ns
		m.Rcode = resp.Rcode

		return
	}

	m.Rcode = dns.RcodeServerFailure
}

func aRecord(name string, a netip.Addr) *dns.A {
	return &dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0, Rdlength: 0},
		A:   a.AsSlice(),
	}
}

// cappedListener counts accepted connections against the TCP cap and closes connections past it.
type cappedListener struct {
	net.Listener

	limit *limiter
}

func (l *cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err //nolint:wrapcheck // The DNS server only checks the error.
		}

		if l.limit.acquire() {
			return &releaseConn{Conn: c, release: sync.OnceFunc(l.limit.release)}, nil
		}

		_ = c.Close()
	}
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
