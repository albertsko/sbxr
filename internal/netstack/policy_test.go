package netstack

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// hostAddr stands for an address of a host interface in policy tests.
var hostAddr = netip.MustParseAddr("198.51.100.7")

func testPolicy(allowLAN bool) *policy {
	p := newPolicy(allowLAN, []Publish{{Host: netip.MustParseAddrPort("127.0.0.1:8080"), Guest: 80}})
	hosts := map[netip.Addr]bool{hostAddr: true, loopback: true}
	p.hostAddrs.Store(&hosts)

	return p
}

func TestAllowDial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr    string
		want    bool
		wantLAN bool
	}{
		{addr: "203.0.113.10", want: true, wantLAN: true},
		{addr: "8.8.8.8", want: true, wantLAN: true},
		{addr: "100.63.255.255", want: true, wantLAN: true},
		{addr: "100.128.0.0", want: true, wantLAN: true},
		{addr: "172.32.0.1", want: true, wantLAN: true},
		{addr: "10.0.0.1", wantLAN: true},
		{addr: "10.255.255.255", wantLAN: true},
		{addr: "172.16.0.1", wantLAN: true},
		{addr: "172.31.255.254", wantLAN: true},
		{addr: "192.168.1.1", wantLAN: true},
		{addr: "100.64.0.1", wantLAN: true},
		{addr: "100.127.255.254", wantLAN: true},
		{addr: hostAddr.String(), wantLAN: true},
		{addr: "0.0.0.0"},
		{addr: "0.1.2.3"},
		{addr: "127.0.0.1"},
		{addr: "127.255.255.254"},
		{addr: "169.254.169.254"},
		{addr: "169.254.0.1"},
		{addr: "224.0.0.1"},
		{addr: "239.255.255.250"},
		{addr: "240.0.0.1"},
		{addr: "255.255.255.255"},
		{addr: "192.168.127.1"},
		{addr: "192.168.127.2"},
		{addr: "192.168.127.255"},
		{addr: "::1"},
		{addr: "::"},
		{addr: "2001:db8::1"},
		{addr: "::ffff:203.0.113.10"},
		{addr: "fe80::1"},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()

			addr := netip.MustParseAddr(tt.addr)

			if got := testPolicy(false).allowDial(addr); got != tt.want {
				t.Errorf("allowDial = %v, want %v", got, tt.want)
			}

			if got := testPolicy(true).allowDial(addr); got != tt.wantLAN {
				t.Errorf("allowDial with AllowLAN = %v, want %v", got, tt.wantLAN)
			}
		})
	}
}

func TestAllowDialHostAddrsChange(t *testing.T) {
	t.Parallel()

	p := testPolicy(false)
	addr := netip.MustParseAddr("203.0.113.99")

	if !p.allowDial(addr) {
		t.Fatal("public address blocked before it became a host address")
	}

	hosts := map[netip.Addr]bool{addr: true}
	p.hostAddrs.Store(&hosts)

	if p.allowDial(addr) {
		t.Fatal("host address allowed after the refresh")
	}
}

var (
	testGuestMAC   = tcpip.LinkAddress([]byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee})
	testGatewayMAC = tcpip.LinkAddress([]byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd})
	otherMAC       = tcpip.LinkAddress([]byte{0x02, 0, 0, 0, 0, 0x01})
)

// ipv4Frame builds an Ethernet frame from the guest with an IPv4 header and a UDP or TCP header with ports.
// The UDP checksum is zero, which IPv4 accepts, so the frame is valid for the stack too.
func ipv4Frame(dst netip.Addr, proto uint8, srcPort, dstPort uint16, payload []byte) []byte {
	const l4Len = 20 // Room for a TCP header. UDP uses the first 8 bytes.

	b := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+l4Len+len(payload))
	header.Ethernet(b).Encode(&header.EthernetFields{SrcAddr: testGuestMAC, DstAddr: testGatewayMAC, Type: header.IPv4ProtocolNumber})

	ip := header.IPv4(b[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(ip)),
		TTL:         64,
		Protocol:    proto,
		SrcAddr:     tcpAddr(guestIP),
		DstAddr:     tcpAddr(dst),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	l4 := ip[header.IPv4MinimumSize:]
	binary.BigEndian.PutUint16(l4[0:2], srcPort)
	binary.BigEndian.PutUint16(l4[2:4], dstPort)

	if proto == protoUDP {
		ip.SetTotalLength(uint16(header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)))
		ip.SetChecksum(0)
		ip.SetChecksum(^ip.CalculateChecksum())
		binary.BigEndian.PutUint16(l4[4:6], uint16(header.UDPMinimumSize+len(payload)))
		copy(l4[header.UDPMinimumSize:], payload)

		return b[:header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize+len(payload)]
	}

	return b
}

func etherFrame(etherType uint16, payloadLen int) []byte {
	b := make([]byte, header.EthernetMinimumSize+payloadLen)
	header.Ethernet(b).Encode(&header.EthernetFields{SrcAddr: testGuestMAC, DstAddr: testGatewayMAC, Type: tcpip.NetworkProtocolNumber(etherType)})

	return b
}

// arpFrame builds an ARP request with the given Ethernet source and sender addresses.
func arpFrame(src, sender tcpip.LinkAddress, senderIP netip.Addr) []byte {
	b := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(b).Encode(&header.EthernetFields{SrcAddr: src, DstAddr: header.EthernetBroadcastAddress, Type: header.ARPProtocolNumber})

	arp := header.ARP(b[header.EthernetMinimumSize:])
	arp.SetIPv4OverEthernet()
	arp.SetOp(header.ARPRequest)
	copy(arp.HardwareAddressSender(), sender)
	copy(arp.ProtocolAddressSender(), senderIP.AsSlice())
	copy(arp.ProtocolAddressTarget(), gatewayIP.AsSlice())

	return b
}

// spoofMAC sets the Ethernet source address of frame to another MAC address.
func spoofMAC(frame []byte) []byte {
	copy(frame[6:12], otherMAC)

	return frame
}

func modify(b []byte, f func(ip []byte)) []byte {
	f(b[header.EthernetMinimumSize:])

	return b
}

func TestAllowFrame(t *testing.T) {
	t.Parallel()

	public := netip.MustParseAddr("203.0.113.10")
	fragment := func(ip []byte) { ip[6] = 0x20 } // More fragments flag.

	tests := []struct {
		name    string
		frame   []byte
		want    bool
		wantLAN bool
	}{
		{name: "public udp", frame: ipv4Frame(public, protoUDP, 40000, 53, nil), want: true, wantLAN: true},
		{name: "public tcp", frame: ipv4Frame(public, protoTCP, 40000, 443, nil), want: true, wantLAN: true},
		{name: "public icmp", frame: ipv4Frame(public, 1, 0, 0, nil), want: true, wantLAN: true},
		{name: "public fragment", frame: modify(ipv4Frame(public, protoUDP, 1, 2, nil), fragment), want: true, wantLAN: true},
		{name: "public later fragment without ports", frame: modify(ipv4Frame(public, protoUDP, 1, 2, nil), func(ip []byte) { ip[7] = 0x10 })[:header.EthernetMinimumSize+header.IPv4MinimumSize], want: true, wantLAN: true},
		{name: "public with options", frame: modify(ipv4Frame(public, protoTCP, 1, 2, nil), func(ip []byte) { ip[0] = 0x46 }), want: true, wantLAN: true},
		{name: "arp", frame: arpFrame(testGuestMAC, testGuestMAC, guestIP), want: true, wantLAN: true},
		{name: "arp probe", frame: arpFrame(testGuestMAC, testGuestMAC, netip.IPv4Unspecified()), want: true, wantLAN: true},
		{name: "arp from another mac", frame: arpFrame(otherMAC, otherMAC, guestIP)},
		{name: "arp with another sender mac", frame: arpFrame(testGuestMAC, otherMAC, guestIP)},
		{name: "arp with another sender ip", frame: arpFrame(testGuestMAC, testGuestMAC, netip.MustParseAddr("192.168.127.3"))},
		{name: "short arp", frame: arpFrame(testGuestMAC, testGuestMAC, guestIP)[:header.EthernetMinimumSize+27]},
		{name: "spoofed source mac", frame: spoofMAC(ipv4Frame(public, protoTCP, 40000, 443, nil))},
		{name: "spoofed source mac to gateway dns", frame: spoofMAC(ipv4Frame(gatewayIP, protoUDP, 40000, 53, nil))},
		{name: "dhcp to public", frame: ipv4Frame(public, protoUDP, 68, 67, nil)},
		{name: "dhcp later fragment to public", frame: modify(ipv4Frame(public, protoUDP, 68, 67, nil), func(ip []byte) { ip[7] = 1 }), want: true, wantLAN: true},
		{name: "rfc1918", frame: ipv4Frame(netip.MustParseAddr("10.1.2.3"), protoTCP, 1, 80, nil), wantLAN: true},
		{name: "cgnat", frame: ipv4Frame(netip.MustParseAddr("100.64.0.1"), protoUDP, 1, 80, nil), wantLAN: true},
		{name: "host address", frame: ipv4Frame(hostAddr, protoTCP, 1, 80, nil), wantLAN: true},
		{name: "unspecified", frame: ipv4Frame(netip.MustParseAddr("0.0.0.0"), protoUDP, 1, 9, nil)},
		{name: "loopback", frame: ipv4Frame(netip.MustParseAddr("127.0.0.1"), protoTCP, 1, 22, nil)},
		{name: "metadata", frame: ipv4Frame(netip.MustParseAddr("169.254.169.254"), protoTCP, 1, 80, nil)},
		{name: "multicast", frame: ipv4Frame(netip.MustParseAddr("224.0.0.251"), protoUDP, 5353, 5353, nil)},
		{name: "reserved", frame: ipv4Frame(netip.MustParseAddr("240.0.0.1"), protoUDP, 1, 1, nil)},
		{name: "gateway dns udp", frame: ipv4Frame(gatewayIP, protoUDP, 40000, 53, nil), want: true, wantLAN: true},
		{name: "gateway dns tcp", frame: ipv4Frame(gatewayIP, protoTCP, 40000, 53, nil), want: true, wantLAN: true},
		{name: "gateway dhcp renew", frame: ipv4Frame(gatewayIP, protoUDP, 68, 67, nil), want: true, wantLAN: true},
		{name: "gateway reply from published port", frame: ipv4Frame(gatewayIP, protoTCP, 80, 40000, nil), want: true, wantLAN: true},
		{name: "gateway reply from unpublished port", frame: ipv4Frame(gatewayIP, protoTCP, 81, 40000, nil)},
		{name: "gateway udp from published port", frame: ipv4Frame(gatewayIP, protoUDP, 80, 40000, nil)},
		{name: "gateway ssh", frame: ipv4Frame(gatewayIP, protoTCP, 40000, 22, nil)},
		{name: "gateway icmp", frame: ipv4Frame(gatewayIP, 1, 0, 0, nil)},
		{name: "gateway dns fragment", frame: modify(ipv4Frame(gatewayIP, protoUDP, 40000, 53, nil), fragment)},
		{name: "gateway dns later fragment", frame: modify(ipv4Frame(gatewayIP, protoUDP, 40000, 53, nil), func(ip []byte) { ip[7] = 1 })},
		{name: "gateway without ports", frame: ipv4Frame(gatewayIP, protoUDP, 40000, 53, nil)[:header.EthernetMinimumSize+header.IPv4MinimumSize+2]},
		{name: "guest subnet", frame: ipv4Frame(netip.MustParseAddr("192.168.127.3"), protoTCP, 1, 80, nil)},
		{name: "subnet broadcast", frame: ipv4Frame(netip.MustParseAddr("192.168.127.255"), protoUDP, 68, 67, nil)},
		{name: "broadcast dhcp", frame: ipv4Frame(limitedBroadcast, protoUDP, 68, 67, nil), want: true, wantLAN: true},
		{name: "broadcast other udp", frame: ipv4Frame(limitedBroadcast, protoUDP, 68, 53, nil)},
		{name: "broadcast tcp", frame: ipv4Frame(limitedBroadcast, protoTCP, 68, 67, nil)},
		{name: "ipv6", frame: etherFrame(0x86dd, 40)},
		{name: "vlan", frame: etherFrame(0x8100, 46)},
		{name: "empty", frame: nil},
		{name: "short ethernet", frame: make([]byte, 13)},
		{name: "ethernet only", frame: etherFrame(etherTypeIPv4, 0)},
		{name: "short ipv4", frame: etherFrame(etherTypeIPv4, 19)},
		{name: "ipv6 in ipv4 ethertype", frame: modify(ipv4Frame(public, protoUDP, 1, 2, nil), func(ip []byte) { ip[0] = 0x60 })},
		{name: "ihl too small", frame: modify(ipv4Frame(public, protoUDP, 1, 2, nil), func(ip []byte) { ip[0] = 0x44 })},
		{name: "ihl past frame", frame: modify(ipv4Frame(public, protoUDP, 1, 2, nil), func(ip []byte) { ip[0] = 0x4f })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := testPolicy(false).allowFrame(tt.frame); got != tt.want {
				t.Errorf("allowFrame = %v, want %v", got, tt.want)
			}

			if got := testPolicy(true).allowFrame(tt.frame); got != tt.wantLAN {
				t.Errorf("allowFrame with AllowLAN = %v, want %v", got, tt.wantLAN)
			}
		})
	}
}

func FuzzAllowFrame(f *testing.F) {
	f.Add(ipv4Frame(netip.MustParseAddr("203.0.113.10"), protoUDP, 1, 2, []byte("x")))
	f.Add(ipv4Frame(gatewayIP, protoTCP, 80, 40000, nil))
	f.Add(ipv4Frame(limitedBroadcast, protoUDP, 68, 67, nil))
	f.Add(arpFrame(testGuestMAC, testGuestMAC, guestIP))
	f.Add(etherFrame(0x86dd, 40))
	f.Add(ipv4Frame(netip.MustParseAddr("127.0.0.1"), protoTCP, 40000, 22, nil))

	p := testPolicy(false)

	f.Fuzz(func(t *testing.T, frame []byte) {
		if !p.allowFrame(frame) {
			_ = p.reset(frame)

			return
		}

		if !bytes.Equal(frame[6:12], []byte(testGuestMAC)) {
			t.Fatalf("allowed source MAC %x", frame[6:12])
		}

		if typ := binary.BigEndian.Uint16(frame[12:14]); typ != etherTypeARP && typ != etherTypeIPv4 {
			t.Fatalf("allowed ether type %#04x", typ)
		}
	})
}

func TestResetForBlockedSYN(t *testing.T) {
	t.Parallel()

	const (
		syn = uint8(header.TCPFlagSyn)
		ack = uint8(header.TCPFlagAck)
		rst = uint8(header.TCPFlagRst)
		ecn = uint8(header.TCPFlagEce | header.TCPFlagCwr)
	)

	tcpFrame := func(flags uint8) []byte {
		b := ipv4Frame(netip.MustParseAddr("10.0.0.1"), protoTCP, 40000, 80, nil)
		l4 := b[header.EthernetMinimumSize+header.IPv4MinimumSize:]
		binary.BigEndian.PutUint32(l4[4:8], 1000)
		l4[12] = header.TCPMinimumSize << 2
		l4[13] = flags

		return b
	}

	tests := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{name: "syn", frame: tcpFrame(syn), want: true},
		{name: "syn with ecn", frame: tcpFrame(syn | ecn), want: true},
		{name: "syn ack", frame: tcpFrame(syn | ack)},
		{name: "syn rst", frame: tcpFrame(syn | rst)},
		{name: "ack", frame: tcpFrame(ack)},
		{name: "spoofed source mac", frame: spoofMAC(tcpFrame(syn))},
		{name: "not tcp", frame: ipv4Frame(netip.MustParseAddr("10.0.0.1"), protoUDP, 40000, 80, nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := testPolicy(false).reset(tt.frame)
			if (got != nil) != tt.want {
				t.Fatalf("reset = %x, want reset %v", got, tt.want)
			}

			if got == nil {
				return
			}

			ip := header.IPv4(got[header.EthernetMinimumSize:])
			reply := header.TCP(ip.Payload())

			if reply.Flags() != header.TCPFlagRst|header.TCPFlagAck || reply.AckNumber() != 1001 || reply.DestinationPort() != 40000 {
				t.Errorf("reset flags %v, ack %d, port %d", reply.Flags(), reply.AckNumber(), reply.DestinationPort())
			}

			if !ip.IsChecksumValid() || !reply.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), 0, 0) {
				t.Error("reset has an invalid checksum")
			}
		})
	}
}
