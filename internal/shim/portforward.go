//go:build linux

package shim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/podnet"
	"github.com/albertsko/sbxr/internal/podvm"
)

// portForward makes kubectl port-forward reach the guest.
//
// containerd forwards a port by dialing localhost in the pod network namespace, where nothing listens now that the guest owns the pod address.
// An iptables rule there redirects every local TCP connection to one listener of the shim, which reads the original port
// and opens a port-forward stream to the agent.
type portForward struct {
	l     net.Listener
	netns string
	port  string
}

func (s *service) listenPortForward(sb *sandbox) (*portForward, error) {
	var l net.Listener

	err := podnet.Do(sb.netns, func() error {
		var err error

		var lc net.ListenConfig

		l, err = lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")

		return err //nolint:wrapcheck // Wrapped below.
	})
	if err != nil {
		return nil, fmt.Errorf("listen in pod network namespace: %w", err)
	}

	addr, _ := l.Addr().(*net.TCPAddr)
	pf := &portForward{l: l, netns: sb.netns, port: strconv.Itoa(addr.Port)}

	err = pf.iptables("-A")
	if err != nil {
		_ = l.Close()

		return nil, err
	}

	go s.servePortForward(sb, l)

	return pf, nil
}

// iptables adds (-A) or deletes (-D) the redirect rule.
func (pf *portForward) iptables(op string) error {
	//nolint:gosec // Fixed command with the pod's namespace path and a port number.
	out, err := exec.CommandContext(context.Background(), "nsenter", "--net="+pf.netns, "iptables", "-w", "-t", "nat", op, "OUTPUT",
		"-o", "lo", "-p", "tcp", "!", "--dport", pf.port, "-j", "REDIRECT", "--to-ports", pf.port).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables redirect %s: %w: %s", op, err, out)
	}

	return nil
}

// close stops the listener and removes the rule.
func (pf *portForward) close() error {
	return errors.Join(pf.l.Close(), pf.iptables("-D"))
}

func (s *service) servePortForward(sb *sandbox, l net.Listener) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			continue
		}

		go func() {
			defer conn.Close() //nolint:errcheck // The stream ends either way.

			port, err := originalPort(conn)
			if err != nil {
				s.log.Warn("port-forward", "error", err)

				return
			}

			guest, err := sb.dialStream(podvm.Header{Kind: podvm.PortForward, Container: "", Exec: "", Port: port})
			if err != nil {
				s.log.Warn("port-forward", "error", err)

				return
			}
			defer guest.Close() //nolint:errcheck // See above.

			done := make(chan struct{}, 2)

			go func() { _, _ = io.Copy(guest, conn); done <- struct{}{} }()
			go func() { _, _ = io.Copy(conn, guest); done <- struct{}{} }()

			<-done
		}()
	}
}

// originalPort returns the destination port of a connection before the REDIRECT rule.
func originalPort(conn net.Conn) (uint16, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return 0, errors.New("original port: not a TCP connection")
	}

	raw, err := tcp.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("original port: %w", err)
	}

	var (
		addr    *unix.IPv6Mreq
		sockErr error
	)

	err = raw.Control(func(fd uintptr) {
		// SO_ORIGINAL_DST returns a sockaddr_in, which has the size of an IPv6Mreq.
		addr, sockErr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
	})
	if err = errors.Join(err, sockErr); err != nil {
		return 0, fmt.Errorf("original port: %w", err)
	}

	return binary.BigEndian.Uint16(addr.Multiaddr[2:4]), nil
}
