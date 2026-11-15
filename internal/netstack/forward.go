package netstack

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const udpBufSize = 65535

// limiter counts the flows of one protocol against its cap.
type limiter struct {
	active atomic.Int64
	max    int64
}

func newLimiter(limit, fallback int) limiter {
	return limiter{active: atomic.Int64{}, max: int64(cmp.Or(limit, fallback))}
}

func (l *limiter) acquire() bool {
	for {
		cur := l.active.Load()
		if cur >= l.max {
			return false
		}

		if l.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (l *limiter) release() { l.active.Add(-1) }

// forwardTCP handles a guest SYN that matches no endpoint. gvisor runs it in its own goroutine, so the relay runs here.
func (n *Network) forwardTCP(r *tcp.ForwarderRequest) {
	dst := destination(r.ID())
	if !n.policy.allowDial(dst.Addr()) || !n.enter() {
		r.Complete(true)
		return
	}
	defer n.wg.Done()

	if !n.tcp.acquire() {
		n.logger.Debug("netstack: tcp cap reached", "dst", dst)
		r.Complete(true)

		return
	}
	defer n.tcp.release()

	ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
	out, err := n.dial(ctx, "tcp", dst.String())

	cancel()

	if err != nil {
		n.logger.Debug("netstack: dial failed", "dst", dst, "err", err)
		r.Complete(true)

		return
	}

	var wq waiter.Queue

	ep, tcpErr := r.CreateEndpoint(&wq)
	r.Complete(false)

	if tcpErr != nil {
		_ = out.Close()
		return
	}

	relayTCP(n.ctx, gonet.NewTCPConn(&wq, ep), out)
}

// forwardUDP handles a guest datagram that matches no endpoint, which is the first datagram of a flow.
// It runs in the packet path, so the relay gets its own goroutine. It returns true, so unhandled datagrams are dropped.
func (n *Network) forwardUDP(r *udp.ForwarderRequest) bool {
	dst := destination(r.ID())
	if !n.policy.allowDial(dst.Addr()) || !n.enter() {
		return true
	}

	if !n.udp.acquire() {
		n.logger.Debug("netstack: udp cap reached", "dst", dst)
		n.wg.Done()

		return true
	}

	out, err := n.dial(n.ctx, "udp", dst.String())
	if err != nil {
		n.logger.Debug("netstack: dial failed", "dst", dst, "err", err)
		n.udp.release()
		n.wg.Done()

		return true
	}

	var wq waiter.Queue

	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		_ = out.Close()

		n.udp.release()
		n.wg.Done()

		return true
	}

	go func() {
		defer n.wg.Done()
		defer n.udp.release()

		relayUDP(n.ctx, gonet.NewUDPConn(&wq, ep), out, n.udpIdle)
	}()

	return true
}

func destination(id stack.TransportEndpointID) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
}

// relayTCP copies both ways and returns when both directions end or ctx ends.
// A clean end of one direction is passed on as a half close. An error closes both connections.
func relayTCP(ctx context.Context, a, b net.Conn) {
	closeBoth := sync.OnceFunc(func() {
		_ = a.Close()
		_ = b.Close()
	})
	defer closeBoth()

	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()

	half := func(dst, src net.Conn) {
		if _, err := io.Copy(dst, src); err != nil {
			closeBoth()
			return
		}

		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}

	var wg sync.WaitGroup

	wg.Go(func() { half(b, a) })
	half(a, b)
	wg.Wait()
}

// relayUDP copies datagrams both ways until the flow is idle for idle, either side fails, or ctx ends.
func relayUDP(ctx context.Context, guest, out net.Conn, idle time.Duration) {
	closeBoth := sync.OnceFunc(func() {
		_ = guest.Close()
		_ = out.Close()
	})
	defer closeBoth()

	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()

	var last atomic.Int64

	last.Store(time.Now().UnixNano())

	pump := func(dst, src net.Conn) {
		defer closeBoth()

		buf := make([]byte, udpBufSize)

		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))

			n, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
					continue
				}

				return
			}

			last.Store(time.Now().UnixNano())

			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup

	wg.Go(func() { pump(out, guest) })
	pump(guest, out)
	wg.Wait()
}
