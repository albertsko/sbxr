package netstack

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"syscall"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// sockBufSize is the send and receive buffer of both socket ends.
// On macOS the send buffer of a datagram socket also limits the frame size, and libkrun does not size an fd it is given.
const sockBufSize = 4 << 20

// socketpair returns the host and VM ends of a datagram socket pair. Both ends are close-on-exec.
func socketpair() (host, vm *os.File, err error) {
	syscall.ForkLock.RLock()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}

	syscall.ForkLock.RUnlock()

	if err != nil {
		return nil, nil, fmt.Errorf("network socket: %w", os.NewSyscallError("socketpair", err))
	}

	host = os.NewFile(uintptr(fds[0]), "netstack-host")
	vm = os.NewFile(uintptr(fds[1]), "netstack-vm")

	for _, fd := range fds {
		for _, opt := range []int{syscall.SO_SNDBUF, syscall.SO_RCVBUF} {
			if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, sockBufSize); err != nil {
				_ = host.Close()
				_ = vm.Close()

				return nil, nil, fmt.Errorf("network socket: %w", os.NewSyscallError("setsockopt", err))
			}
		}
	}

	return host, vm, nil
}

// filterConn is the host end of the VM socket. It drops guest frames that the egress policy rejects before the switch sees them.
type filterConn struct {
	net.Conn

	policy *policy
}

// vmAddr names the unnamed socket pair end, because the switch logs the addresses of its connections.
var vmAddr = &net.UnixAddr{Name: "vm", Net: "unixgram"}

func (*filterConn) LocalAddr() net.Addr  { return vmAddr }
func (*filterConn) RemoteAddr() net.Addr { return vmAddr }

func (c *filterConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil || c.policy.allowFrame(b[:n]) {
			return n, err //nolint:wrapcheck // The switch only logs the error.
		}

		if rst := c.policy.reset(b[:n]); rst != nil {
			_, _ = c.Write(rst)
		}
	}
}

// reset returns a TCP reset for a blocked guest SYN, so the guest connection fails at once instead of timing out.
// It returns nil for every other frame.
func (p *policy) reset(frame []byte) []byte {
	if len(frame) < etherHeaderLen || !bytes.Equal(frame[6:12], p.guestMAC) || header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		return nil
	}

	ip := header.IPv4(frame[etherHeaderLen:])
	if !ip.IsValid(len(ip)) || ip.TransportProtocol() != header.TCPProtocolNumber || ip.More() || ip.FragmentOffset() != 0 {
		return nil
	}

	syn := header.TCP(ip.Payload())
	if len(syn) < header.TCPMinimumSize || syn.Flags()&(header.TCPFlagSyn|header.TCPFlagAck|header.TCPFlagRst) != header.TCPFlagSyn {
		return nil
	}

	b := make([]byte, etherHeaderLen+header.IPv4MinimumSize+header.TCPMinimumSize)
	header.Ethernet(b).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(gatewayHWAddr),
		DstAddr: tcpip.LinkAddress(p.guestMAC),
		Type:    header.IPv4ProtocolNumber,
	})

	rip := header.IPv4(b[etherHeaderLen:])
	rip.Encode(&header.IPv4Fields{
		TotalLength: header.IPv4MinimumSize + header.TCPMinimumSize,
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     ip.DestinationAddress(),
		DstAddr:     ip.SourceAddress(),
	})
	rip.SetChecksum(^rip.CalculateChecksum())

	rst := header.TCP(rip[header.IPv4MinimumSize:])
	rst.Encode(&header.TCPFields{
		SrcPort:    syn.DestinationPort(),
		DstPort:    syn.SourcePort(),
		AckNum:     syn.SequenceNumber() + 1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagRst | header.TCPFlagAck,
	})

	sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, rip.SourceAddress(), rip.DestinationAddress(), header.TCPMinimumSize)
	rst.SetChecksum(^rst.CalculateChecksum(sum))

	return b
}
