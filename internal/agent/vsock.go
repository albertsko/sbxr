//go:build linux

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// vsockListener accepts vsock connections. The net package does not support AF_VSOCK.
type vsockListener struct {
	fd     int
	port   uint32
	closed atomic.Bool
}

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}

	err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port, Flags: 0})
	if err == nil {
		err = unix.Listen(fd, 64)
	}

	if err != nil {
		_ = unix.Close(fd)

		return nil, fmt.Errorf("listen on vsock port %d: %w", port, err)
	}

	return &vsockListener{fd: fd, port: port, closed: atomic.Bool{}}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		fd, _, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
			continue
		}

		if l.closed.Load() {
			if err == nil {
				_ = unix.Close(fd)
			}

			return nil, net.ErrClosed
		}

		if err != nil {
			return nil, fmt.Errorf("accept on vsock port %d: %w", l.port, err)
		}

		// A non-blocking descriptor joins the runtime poller, so reads and deadlines do not block a thread.
		return &vsockConn{File: os.NewFile(uintptr(fd), "vsock")}, nil
	}
}

// Close stops the listener. Shutdown wakes a blocked Accept, which close alone does not. The descriptor stays open,
// so that its number cannot go to another file while Accept still uses it.
func (l *vsockListener) Close() error {
	if l.closed.Swap(true) {
		return net.ErrClosed
	}

	return unix.Shutdown(l.fd, unix.SHUT_RDWR) //nolint:wrapcheck // Closing a listener.
}

func (l *vsockListener) Addr() net.Addr { return vsockAddr(l.port) }

// vsockConn is a vsock connection as a net.Conn.
type vsockConn struct {
	*os.File
}

func (c *vsockConn) LocalAddr() net.Addr  { return vsockAddr(0) }
func (c *vsockConn) RemoteAddr() net.Addr { return vsockAddr(0) }

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }      //nolint:wrapcheck // Passthrough.
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }  //nolint:wrapcheck // Passthrough.
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) } //nolint:wrapcheck // Passthrough.

// CloseWrite ends the stream in one direction, so the host sees EOF and can still send.
func (c *vsockConn) CloseWrite() error {
	raw, err := c.SyscallConn()
	if err != nil {
		return fmt.Errorf("vsock close write: %w", err)
	}

	var shutErr error

	err = raw.Control(func(fd uintptr) { shutErr = unix.Shutdown(int(fd), unix.SHUT_WR) })
	if err != nil {
		return fmt.Errorf("vsock close write: %w", err)
	}

	if shutErr != nil {
		return fmt.Errorf("vsock close write: %w", shutErr)
	}

	return nil
}

type vsockAddr uint32

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("vsock:%d", uint32(a)) }
