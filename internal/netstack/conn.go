package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
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
	}
}

// resolver answers names in hosts itself and asks the host resolver for everything else.
type resolver struct {
	*net.Resolver

	hosts map[string]netip.Addr
}

func (r *resolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if a, ok := r.hosts[strings.TrimSuffix(strings.ToLower(host), ".")]; ok {
		return []net.IPAddr{{IP: a.AsSlice()}}, nil
	}

	return r.Resolver.LookupIPAddr(ctx, host) //nolint:wrapcheck // The DNS server only reports the error to the guest.
}
