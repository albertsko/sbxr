package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"
)

// eventually retries f until it returns nil or timeout passes, and returns the last error.
func eventually(timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)

	for {
		err := f()
		if err == nil || time.Now().After(deadline) {
			return err
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestTCPCap(t *testing.T) {
	t.Parallel()

	d := newTestDialer(t)
	n, g := startNetwork(t, Config{
		MaxTCP:  2,
		Dial:    d.dial,
		Hosts:   map[string]netip.Addr{testHost: publicAddr},
		Publish: []Publish{{Guest: 80}},
	})
	dst := netip.AddrPortFrom(publicAddr, 80).String()

	held := make([]net.Conn, 2)

	for i := range held {
		c, err := dialTCP(t, g, dst, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		if err := roundTrip(c, "held", 5*time.Second); err != nil {
			t.Fatal(err)
		}

		held[i] = c
	}

	// Past the cap the guest gets a reset, so the dial fails at once instead of timing out.
	start := time.Now()

	if c, err := dialTCP(t, g, dst, 5*time.Second); err == nil {
		if err := roundTrip(c, "over", time.Second); err == nil {
			t.Fatal("connection past the cap works")
		}
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("connection past the cap failed after %v, want a reset", elapsed)
	}

	// A published connection past the cap gets a reset too.
	var nd net.Dialer

	pub, err := nd.DialContext(t.Context(), "tcp", n.Published()[0].Host.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	_ = pub.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := pub.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("published connection past the cap: read error %v, want a reset", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := g.LookupHost(ctx, testHost); err != nil {
		t.Errorf("DNS at the TCP cap: %v", err)
	}

	_ = held[0].Close()

	err = eventually(5*time.Second, func() error {
		c, err := dialTCP(t, g, dst, time.Second)
		if err != nil {
			return err
		}

		return roundTrip(c, "after", time.Second)
	})
	if err != nil {
		t.Errorf("connection after a slot was released: %v", err)
	}
}

func TestUDPCap(t *testing.T) {
	t.Parallel()

	d := newTestDialer(t)
	_, g := startNetwork(t, Config{MaxUDP: 2, Dial: d.dial, udpIdle: 300 * time.Millisecond})
	dst := netip.AddrPortFrom(publicAddr, 53).String()

	for range 2 {
		if err := roundTrip(dialUDP(t, g, dst), "flow", 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	if err := roundTrip(dialUDP(t, g, dst), "over", 200*time.Millisecond); err == nil {
		t.Fatal("flow past the cap works")
	}

	// The idle timeout ends the first flows and releases their slots.
	err := eventually(5*time.Second, func() error {
		return roundTrip(dialUDP(t, g, dst), "after", 200*time.Millisecond)
	})
	if err != nil {
		t.Errorf("flow after the idle timeout: %v", err)
	}
}

func TestPublish(t *testing.T) {
	t.Parallel()

	n, g := startNetwork(t, Config{Publish: []Publish{
		{Host: netip.AddrPortFrom(netip.Addr{}, 0), Guest: 80},
		{Host: netip.MustParseAddrPort("127.0.0.1:0"), Guest: 81},
	}})

	for _, port := range []uint16{80, 81, 82} {
		ln, err := g.ListenTCP(port)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = ln.Close() })

		go serve(ln, func(c net.Conn) { _, _ = c.Write([]byte{byte(port)}) })
	}

	published := n.Published()
	if len(published) != 2 {
		t.Fatalf("Published = %v, want 2 forwards", published)
	}

	for _, p := range published {
		if p.Host.Addr() != loopback || p.Host.Port() == 0 {
			t.Errorf("published %v, want 127.0.0.1 and a bound port", p)
		}

		var nd net.Dialer

		c, err := nd.DialContext(t.Context(), "tcp", p.Host.String())
		if err != nil {
			t.Fatal(err)
		}

		_ = c.SetDeadline(time.Now().Add(5 * time.Second))

		buf := make([]byte, 1)
		if _, err := c.Read(buf); err != nil || buf[0] != byte(p.Guest) {
			t.Errorf("%v: read %v, %v, want guest port %d", p, buf, err, p.Guest)
		}

		_ = c.Close()
	}

	// The default bind is loopback only, so other host addresses do not answer on the published port.
	if host, ok := firstHostIPv4(); ok {
		var nd net.Dialer

		addr := netip.AddrPortFrom(host, published[0].Host.Port()).String()
		if c, err := nd.DialContext(t.Context(), "tcp", addr); err == nil {
			_ = c.Close()

			t.Errorf("published port answers on %s", addr)
		}
	}

	if got, want := published[0].String(), published[0].Host.String()+" -> 80/tcp"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestPublishClosedGuestPort(t *testing.T) {
	t.Parallel()

	n, _ := startNetwork(t, Config{Publish: []Publish{{Guest: 80}}})

	var nd net.Dialer

	c, err := nd.DialContext(t.Context(), "tcp", n.Published()[0].Host.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("read from a guest port without listener succeeded")
	}
}

func TestStartRejectsInvalidPublish(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	busy, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = busy.Close() })

	tests := map[string]Publish{
		"ipv6 host":  {Host: netip.MustParseAddrPort("[::1]:0"), Guest: 80},
		"guest 0":    {Host: netip.MustParseAddrPort("127.0.0.1:0")},
		"port taken": {Host: netip.MustParseAddrPort(busy.Addr().String()), Guest: 80},
	}

	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			n, err := Start(t.Context(), Config{Publish: []Publish{{Guest: 80}, p}})
			if err == nil {
				_ = n.Close()

				t.Fatal("Start succeeded")
			}
		})
	}
}
