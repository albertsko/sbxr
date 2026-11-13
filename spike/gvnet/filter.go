package main

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync/atomic"
)

// guestConn wraps the VM datagram socket. It drops guest frames to blocked IPv4 destinations before gvisor sees them.
type guestConn struct {
	net.Conn
	blocked []netip.Prefix
	dropped atomic.Uint64
}

var fakeAddr = &net.UnixAddr{Name: "vm", Net: "unixgram"}

func (c *guestConn) LocalAddr() net.Addr  { return fakeAddr }
func (c *guestConn) RemoteAddr() net.Addr { return fakeAddr }

func (c *guestConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil || !c.drop(b[:n]) {
			return n, err
		}
		c.dropped.Add(1)
	}
}

func (c *guestConn) drop(frame []byte) bool {
	if len(c.blocked) == 0 || len(frame) < 14+20 {
		return false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return false
	}
	dst := netip.AddrFrom4([4]byte(frame[14+16 : 14+20]))
	for _, p := range c.blocked {
		if p.Contains(dst) {
			return true
		}
	}
	return false
}
