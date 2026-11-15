package netstack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
)

const (
	etherHeaderLen = 14
	etherTypeIPv4  = 0x0800
	etherTypeARP   = 0x0806
	ipv4MinLen     = 20
	arpLen         = 28
	protoTCP       = 6
	protoUDP       = 17
	portDNS        = 53
	portDHCP       = 67
)

var (
	alwaysBlocked = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	lanBlocked = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
	}
	limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

// policy is the egress policy from the PRD "Networking (CLI)" section.
// The frame filter and the forwarders use the same policy.
type policy struct {
	allowLAN bool
	subnet   netip.Prefix
	gateway  netip.Addr
	guestIP  netip.Addr
	guestMAC net.HardwareAddr

	// published holds the guest TCP ports that the host can reach. The guest answers those connections from the published port to the gateway.
	published map[uint16]bool

	hostAddrs atomic.Pointer[map[netip.Addr]bool]
}

// refreshHostAddrs reloads the host interface addresses, because DHCP or a VPN can change them while the VM runs.
func (p *policy) refreshHostAddrs() error {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("read host addresses: %w", err)
	}

	set := make(map[netip.Addr]bool, len(addrs))

	for _, a := range addrs {
		if pfx, err := netip.ParsePrefix(a.String()); err == nil {
			set[pfx.Addr().Unmap()] = true
		}
	}

	p.hostAddrs.Store(&set)

	return nil
}

// allowDial reports whether a guest connection to dst may leave the host.
// The gateway and the rest of the guest subnet are never dialed, because only the gateway services live there.
func (p *policy) allowDial(dst netip.Addr) bool {
	if !dst.Is4() || p.subnet.Contains(dst) || dst == limitedBroadcast {
		return false
	}

	for _, pfx := range alwaysBlocked {
		if pfx.Contains(dst) {
			return false
		}
	}

	if hosts := p.hostAddrs.Load(); hosts != nil && (*hosts)[dst] {
		return p.allowLAN
	}

	for _, pfx := range lanBlocked {
		if pfx.Contains(dst) {
			return p.allowLAN
		}
	}

	return true
}

// allowFrame reports whether a guest Ethernet frame may reach the network stack.
func (p *policy) allowFrame(frame []byte) bool {
	if len(frame) < etherHeaderLen || !bytes.Equal(frame[6:12], p.guestMAC) {
		return false
	}

	switch binary.BigEndian.Uint16(frame[12:14]) {
	case etherTypeARP:
		return p.allowARP(frame[etherHeaderLen:])
	case etherTypeIPv4:
		return p.allowIPv4(frame[etherHeaderLen:])
	default:
		return false
	}
}

// allowARP accepts ARP only for the guest's own addresses, so the switch learns no other MAC address.
func (p *policy) allowARP(pkt []byte) bool {
	if len(pkt) < arpLen {
		return false
	}

	senderIP := netip.AddrFrom4([4]byte(pkt[14:18]))

	return bytes.Equal(pkt[8:14], p.guestMAC) && (senderIP == p.guestIP || senderIP == netip.IPv4Unspecified())
}

func (p *policy) allowIPv4(pkt []byte) bool {
	if len(pkt) < ipv4MinLen || pkt[0]>>4 != 4 {
		return false
	}

	hdrLen := int(pkt[0]&0x0f) * 4
	if hdrLen < ipv4MinLen || len(pkt) < hdrLen {
		return false
	}

	dst := netip.AddrFrom4([4]byte(pkt[16:20]))
	proto := pkt[9]

	// Only a frame that is not fragmented has both ports, and only the first fragment has any.
	whole := binary.BigEndian.Uint16(pkt[6:8])&0x3fff == 0 && len(pkt) >= hdrLen+4
	firstFragment := binary.BigEndian.Uint16(pkt[6:8])&0x1fff == 0 && len(pkt) >= hdrLen+4

	if dst != p.gateway && dst != limitedBroadcast {
		// DHCP goes only to the gateway or as broadcast.
		if proto == protoUDP && firstFragment && binary.BigEndian.Uint16(pkt[hdrLen+2:hdrLen+4]) == portDHCP {
			return false
		}

		return p.allowDial(dst)
	}

	// Gateway and broadcast traffic needs port checks, so fragments that can hide the ports are dropped.
	if !whole {
		return false
	}

	srcPort := binary.BigEndian.Uint16(pkt[hdrLen : hdrLen+2])
	dstPort := binary.BigEndian.Uint16(pkt[hdrLen+2 : hdrLen+4])

	if dst == limitedBroadcast {
		return proto == protoUDP && dstPort == portDHCP
	}

	switch proto {
	case protoUDP:
		return dstPort == portDNS || dstPort == portDHCP
	case protoTCP:
		return dstPort == portDNS || p.published[srcPort]
	default:
		return false
	}
}
