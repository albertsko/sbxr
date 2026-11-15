package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

const acceptRetryDelay = 50 * time.Millisecond

// listen binds one publish address and forwards its connections to the guest port.
func (n *Network) listen(p Publish) error {
	var lc net.ListenConfig

	ln, err := lc.Listen(n.ctx, "tcp4", p.Host.String())
	if err != nil {
		return fmt.Errorf("publish %s: %w", p, err)
	}

	n.listeners = append(n.listeners, ln)

	bound, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		return fmt.Errorf("publish %s: %w", p, err)
	}

	p.Host = bound
	n.published = append(n.published, p)

	n.wg.Go(func() { n.accept(ln, p.Guest) })

	return nil
}

func (n *Network) accept(ln net.Listener, guestPort uint16) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			// Errors such as EMFILE are temporary, so the listener stays open.
			n.logger.Debug("netstack: accept failed", "err", err)

			select {
			case <-n.ctx.Done():
				return
			case <-time.After(acceptRetryDelay):
			}

			continue
		}

		if !n.enter() {
			_ = c.Close()
			return
		}

		go func() {
			defer n.wg.Done()

			n.forwardInbound(c, guestPort)
		}()
	}
}

// forwardInbound connects a host connection to the guest port. It counts against the TCP cap like guest connections.
func (n *Network) forwardInbound(c net.Conn, guestPort uint16) {
	if !n.tcp.acquire() {
		n.logger.Debug("netstack: tcp cap reached", "guest_port", guestPort)

		_ = c.Close()

		return
	}
	defer n.tcp.release()

	ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
	dst := tcpip.FullAddress{NIC: nicID, Addr: tcpAddr(guestIP), Port: guestPort}
	g, err := gonet.DialContextTCP(ctx, n.stack, dst, ipv4.ProtocolNumber)

	cancel()

	if err != nil {
		n.logger.Debug("netstack: guest dial failed", "guest_port", guestPort, "err", err)

		_ = c.Close()

		return
	}

	relayTCP(n.ctx, c, g)
}
