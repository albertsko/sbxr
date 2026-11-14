package main

import (
	"encoding/binary"
	"log"
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

// guestConn wraps the VM datagram socket. It drops guest frames that the egress policy rejects before gvisor sees them.
type guestConn struct {
	net.Conn
	policy  *egressPolicy
	dropped atomic.Uint64
}

var fakeAddr = &net.UnixAddr{Name: "vm", Net: "unixgram"}

func (c *guestConn) LocalAddr() net.Addr  { return fakeAddr }
func (c *guestConn) RemoteAddr() net.Addr { return fakeAddr }

func (c *guestConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil || c.policy == nil || c.policy.allow(b[:n]) {
			return n, err
		}
		c.dropped.Add(1)
	}
}

var (
	alwaysBlocked = prefixes("0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4")
	lanPrefixes   = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10")
	broadcast     = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// egressPolicy decides per guest frame. Only ARP and IPv4 pass. IPv4 passes when the destination is public, or private with allowLAN.
type egressPolicy struct {
	allowLAN  bool
	gateway   netip.Addr
	hostAddrs atomic.Pointer[map[netip.Addr]bool]
}

func newEgressPolicy(allowLAN bool, gateway string) *egressPolicy {
	p := &egressPolicy{allowLAN: allowLAN, gateway: netip.MustParseAddr(gateway)}
	p.refreshHostAddrs()
	go func() {
		for range time.Tick(2 * time.Second) {
			p.refreshHostAddrs()
		}
	}()
	return p
}

// refreshHostAddrs reloads the host interface addresses, because DHCP or VPNs can change them while the VM runs.
func (p *egressPolicy) refreshHostAddrs() {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("interface addrs: %v", err)
		return
	}
	m := map[netip.Addr]bool{}
	for _, a := range addrs {
		if pfx, err := netip.ParsePrefix(a.String()); err == nil {
			m[pfx.Addr().Unmap()] = true
		}
	}
	if old := p.hostAddrs.Load(); old == nil || len(*old) != len(m) {
		log.Printf("host addrs: %v", m)
	}
	p.hostAddrs.Store(&m)
}

func (p *egressPolicy) allow(frame []byte) bool {
	if len(frame) < 14 {
		return false
	}
	switch binary.BigEndian.Uint16(frame[12:14]) {
	case 0x0806:
		return true
	case 0x0800:
		return p.allowIPv4(frame[14:])
	default:
		return false
	}
}

func (p *egressPolicy) allowIPv4(ip []byte) bool {
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl {
		return false
	}
	dst := netip.AddrFrom4([4]byte(ip[16:20]))
	proto := ip[9]
	fragmented := binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0

	// The gateway is the gvisor stack itself. Only its DNS and DHCP services are reachable; other ports would be forwarded to the host.
	if dst == p.gateway || dst == broadcast {
		if fragmented || len(ip) < ihl+4 {
			return false
		}
		dport := binary.BigEndian.Uint16(ip[ihl+2 : ihl+4])
		if dst == broadcast {
			return proto == 17 && dport == 67
		}
		return (proto == 17 && (dport == 53 || dport == 67)) || (proto == 6 && dport == 53)
	}
	for _, pfx := range alwaysBlocked {
		if pfx.Contains(dst) {
			return false
		}
	}
	if (*p.hostAddrs.Load())[dst] {
		return p.allowLAN
	}
	for _, pfx := range lanPrefixes {
		if pfx.Contains(dst) {
			return p.allowLAN
		}
	}
	return true
}
