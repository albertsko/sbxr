//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// The raw prober writes Ethernet frames on eth0 with AF_PACKET, so the guest kernel's routing (which never sends 0.0.0.0 or 127/8 out) is bypassed.
// It uses a spare source IP so the guest kernel ignores the replies instead of sending RSTs, and answers ARP for that IP itself.

var gatewayMAC = net.HardwareAddr{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd}

type prober struct {
	fd    int
	ifidx int
	mac   net.HardwareAddr
	src   netip.Addr
	src6  netip.Addr
}

func probe(srcIP string, specs []string) {
	ifc, err := net.InterfaceByName("eth0")
	if err != nil {
		fmt.Println("PROBE eth0:", err)
		return
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		fmt.Println("PROBE socket:", err)
		return
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_ALL), Ifindex: ifc.Index}); err != nil {
		fmt.Println("PROBE bind:", err)
		return
	}
	tv := syscall.NsecToTimeval(int64(200 * time.Millisecond))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	p := &prober{fd: fd, ifidx: ifc.Index, mac: ifc.HardwareAddr, src: netip.MustParseAddr(srcIP), src6: netip.MustParseAddr("fd00::3")}
	for _, spec := range specs {
		proto, addr, _ := strings.Cut(spec, ":")
		ap := netip.MustParseAddrPort(addr)
		var res string
		switch proto {
		case "tcp":
			res = p.tcp(ap)
		case "udp":
			res = p.udp(ap)
		}
		fmt.Printf("PROBE %s:%s %s\n", proto, addr, res)
	}
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (p *prober) send(frame []byte) {
	sa := &syscall.SockaddrLinklayer{Ifindex: p.ifidx, Halen: 6}
	copy(sa.Addr[:], gatewayMAC)
	if err := syscall.Sendto(p.fd, frame, 0, sa); err != nil {
		fmt.Println("PROBE sendto:", err)
	}
}

// reply is a parsed IP packet addressed to our spare source IP.
type reply struct {
	src, dst     netip.Addr
	proto        uint8
	sport, dport uint16
	flags        uint8
	seq          uint32
	payload      []byte
	icmpType     uint8
}

// next reads frames until a packet to the spare source IP arrives or the deadline passes. It answers ARP requests on the way.
func (p *prober) next(deadline time.Time) (reply, bool) {
	buf := make([]byte, 65536)
	for time.Now().Before(deadline) {
		n, _, err := syscall.Recvfrom(p.fd, buf, 0)
		if err != nil || n < 14 {
			continue
		}
		f := buf[:n]
		if net.HardwareAddr(f[6:12]).String() == p.mac.String() {
			continue
		}
		switch binary.BigEndian.Uint16(f[12:14]) {
		case 0x0806:
			p.answerARP(f)
		case 0x0800:
			if r, ok := parse4(f[14:]); ok && r.dst == p.src {
				return r, true
			}
		case 0x86dd:
			if r, ok := parse6(f[14:]); ok && r.dst == p.src6 {
				return r, true
			}
		}
	}
	return reply{}, false
}

func (p *prober) answerARP(f []byte) {
	a := f[14:]
	if len(a) < 28 || binary.BigEndian.Uint16(a[6:8]) != 1 || netip.AddrFrom4([4]byte(a[24:28])) != p.src {
		return
	}
	out := make([]byte, 42)
	copy(out[0:6], f[6:12])
	copy(out[6:12], p.mac)
	binary.BigEndian.PutUint16(out[12:14], 0x0806)
	r := out[14:]
	copy(r[0:6], a[0:6])
	binary.BigEndian.PutUint16(r[6:8], 2)
	copy(r[8:14], p.mac)
	copy(r[14:18], a[24:28])
	copy(r[18:24], a[8:14])
	copy(r[24:28], a[14:18])
	p.send(out)
}

func parse4(ip []byte) (reply, bool) {
	if len(ip) < 20 {
		return reply{}, false
	}
	ihl := int(ip[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if total > len(ip) || ihl > total {
		return reply{}, false
	}
	r := reply{src: netip.AddrFrom4([4]byte(ip[12:16])), dst: netip.AddrFrom4([4]byte(ip[16:20])), proto: ip[9]}
	return parseL4(r, ip[ihl:total])
}

func parse6(ip []byte) (reply, bool) {
	if len(ip) < 40 {
		return reply{}, false
	}
	r := reply{src: netip.AddrFrom16([16]byte(ip[8:24])), dst: netip.AddrFrom16([16]byte(ip[24:40])), proto: ip[6]}
	return parseL4(r, ip[40:])
}

func parseL4(r reply, l4 []byte) (reply, bool) {
	switch r.proto {
	case 6:
		if len(l4) < 20 {
			return r, false
		}
		r.sport, r.dport = binary.BigEndian.Uint16(l4[0:2]), binary.BigEndian.Uint16(l4[2:4])
		r.seq = binary.BigEndian.Uint32(l4[4:8])
		r.flags = l4[13]
		r.payload = append([]byte(nil), l4[int(l4[12]>>4)*4:]...)
	case 17:
		if len(l4) < 8 {
			return r, false
		}
		r.sport, r.dport = binary.BigEndian.Uint16(l4[0:2]), binary.BigEndian.Uint16(l4[2:4])
		r.payload = append([]byte(nil), l4[8:]...)
	case 1, 58:
		if len(l4) < 1 {
			return r, false
		}
		r.icmpType = l4[0]
	}
	return r, true
}

const (
	finFlag = 0x01
	synFlag = 0x02
	rstFlag = 0x04
	pshFlag = 0x08
	ackFlag = 0x10
)

func (p *prober) tcp(dst netip.AddrPort) string {
	sport := uint16(20000 + rand.IntN(30000))
	iss := rand.Uint32()
	p.send(p.frame(dst, 6, p.tcpSeg(sport, dst.Port(), iss, 0, synFlag, nil)))

	deadline := time.Now().Add(5 * time.Second)
	for {
		r, ok := p.next(deadline)
		if !ok {
			return "NO-REPLY"
		}
		if r.proto == 1 || r.proto == 58 {
			return fmt.Sprintf("ICMP type=%d from %s", r.icmpType, r.src)
		}
		if r.proto != 6 || r.dport != sport {
			continue
		}
		if r.flags&rstFlag != 0 {
			return "RST (refused)"
		}
		if r.flags&(synFlag|ackFlag) != synFlag|ackFlag {
			continue
		}
		return p.established(dst, sport, iss+1, r.seq+1)
	}
}

// established ACKs the SYN-ACK, waits for the host banner, then resets the connection.
func (p *prober) established(dst netip.AddrPort, sport uint16, seq, ack uint32) string {
	p.send(p.frame(dst, 6, p.tcpSeg(sport, dst.Port(), seq, ack, ackFlag, nil)))
	defer p.send(p.frame(dst, 6, p.tcpSeg(sport, dst.Port(), seq, 0, rstFlag, nil)))

	deadline := time.Now().Add(3 * time.Second)
	for {
		r, ok := p.next(deadline)
		if !ok {
			return "CONNECTED (no data)"
		}
		if r.proto != 6 || r.dport != sport {
			continue
		}
		if r.flags&rstFlag != 0 {
			return "CONNECTED then RST"
		}
		if len(r.payload) > 0 {
			return fmt.Sprintf("CONNECTED data=%q", strings.TrimSpace(string(r.payload)))
		}
		if r.flags&finFlag != 0 {
			return "CONNECTED then FIN (no data)"
		}
	}
}

func (p *prober) udp(dst netip.AddrPort) string {
	sport := uint16(20000 + rand.IntN(30000))
	payload := []byte("ping\n")
	seg := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(seg[0:2], sport)
	binary.BigEndian.PutUint16(seg[2:4], dst.Port())
	binary.BigEndian.PutUint16(seg[4:6], uint16(len(seg)))
	copy(seg[8:], payload)
	p.send(p.frame(dst, 17, seg))

	deadline := time.Now().Add(4 * time.Second)
	for {
		r, ok := p.next(deadline)
		if !ok {
			return "NO-REPLY"
		}
		if r.proto == 1 || r.proto == 58 {
			return fmt.Sprintf("ICMP type=%d from %s", r.icmpType, r.src)
		}
		if r.proto == 17 && r.dport == sport {
			return fmt.Sprintf("REPLY data=%q", strings.TrimSpace(string(r.payload)))
		}
	}
}

func (p *prober) tcpSeg(sport, dport uint16, seq, ack uint32, flags uint8, payload []byte) []byte {
	seg := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(seg[0:2], sport)
	binary.BigEndian.PutUint16(seg[2:4], dport)
	binary.BigEndian.PutUint32(seg[4:8], seq)
	binary.BigEndian.PutUint32(seg[8:12], ack)
	seg[12] = 5 << 4
	seg[13] = flags
	binary.BigEndian.PutUint16(seg[14:16], 64240)
	copy(seg[20:], payload)
	return seg
}

// frame builds an Ethernet frame with an IPv4 or IPv6 header and fills in the L4 checksum.
func (p *prober) frame(dst netip.AddrPort, proto uint8, l4 []byte) []byte {
	csumOff := 16
	if proto == 17 {
		csumOff = 6
	}
	eth := make([]byte, 14)
	copy(eth[0:6], gatewayMAC)
	copy(eth[6:12], p.mac)

	if dst.Addr().Is6() {
		binary.BigEndian.PutUint16(eth[12:14], 0x86dd)
		ip := make([]byte, 40)
		ip[0] = 6 << 4
		binary.BigEndian.PutUint16(ip[4:6], uint16(len(l4)))
		ip[6], ip[7] = proto, 64
		s, d := p.src6.As16(), dst.Addr().As16()
		copy(ip[8:24], s[:])
		copy(ip[24:40], d[:])
		pseudo := append(append(append([]byte{}, s[:]...), d[:]...), 0, 0, byte(len(l4)>>8), byte(len(l4)), 0, 0, 0, proto)
		binary.BigEndian.PutUint16(l4[csumOff:], checksum(append(pseudo, l4...)))
		return append(append(eth, ip...), l4...)
	}

	binary.BigEndian.PutUint16(eth[12:14], 0x0800)
	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(l4)))
	binary.BigEndian.PutUint16(ip[4:6], uint16(rand.IntN(65535)))
	ip[6] = 0x40
	ip[8], ip[9] = 64, proto
	s, d := p.src.As4(), dst.Addr().As4()
	copy(ip[12:16], s[:])
	copy(ip[16:20], d[:])
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	pseudo := append(append(append([]byte{}, s[:]...), d[:]...), 0, proto, byte(len(l4)>>8), byte(len(l4)))
	binary.BigEndian.PutUint16(l4[csumOff:], checksum(append(pseudo, l4...)))
	return append(append(eth, ip...), l4...)
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
