package scenario

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

// hostBody is the response of the host test server.
const hostBody = "from host\n"

// reachHost runs wget in the guest against an HTTP server on a non-loopback host address.
// With allowLAN, the guest must get the response. Without it, the request must fail and never reach the server.
func reachHost(allowLAN bool) func(context.Context, Target) error {
	return func(ctx context.Context, t Target) error {
		addr, requests, stop, err := hostServer(ctx)
		if err != nil {
			return err
		}
		defer stop()

		w := Workload{Image: Busybox, Net: true, AllowLAN: allowLAN, Args: wget("http://" + addr + "/")}

		if allowLAN {
			return expect(w, 0, OK, stdout(hostBody))(ctx, t)
		}

		err = expect(w, 1, OK)(ctx, t)
		if err != nil {
			return err
		}

		if n := requests(); n > 0 {
			return fmt.Errorf("the host server got %d requests from the guest", n)
		}

		return nil
	}
}

// wget returns the arguments that get url with busybox wget and print the body.
func wget(url string) []string { return []string{"wget", "-T", "5", "-qO-", url} }

// hostServer serves hostBody over HTTP on the first non-loopback IPv4 address of the host.
// It never uses a public address: the egress policy treats the host's own addresses as LAN addresses.
func hostServer(ctx context.Context) (addr string, requests func() int64, stop func(), err error) {
	host, ok := hostIPv4()
	if !ok {
		return "", nil, nil, skipError("the host has no non-loopback IPv4 address")
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp4", netip.AddrPortFrom(host, 0).String())
	if err != nil {
		return "", nil, nil, fmt.Errorf("host server: %w", err)
	}

	var n atomic.Int64

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)

			_, _ = io.WriteString(w, hostBody)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() { _ = srv.Serve(ln) }() // Serve ends at Close.

	return ln.Addr().String(), n.Load, func() { _ = srv.Close() }, nil
}

func hostIPv4() (netip.Addr, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, false
	}

	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err == nil && pfx.Addr().Is4() && !pfx.Addr().IsLoopback() && !pfx.Addr().IsLinkLocalUnicast() {
			return pfx.Addr(), true
		}
	}

	return netip.Addr{}, false
}

// publishReachesGuest publishes a guest port on a free host port of 127.0.0.1 and reads what the guest sends.
func publishReachesGuest(ctx context.Context, t Target) error {
	const msg = "published\n"

	type result struct {
		addr string
		got  string
	}

	results := make(chan result, 1)

	w := Workload{
		Image:       Busybox,
		Net:         true,
		Publish:     []uint16{8080},
		Interactive: true,
		Stdin:       msg,
		Args:        []string{"nc", "-l", "-p", "8080"},
		Published: func(ctx context.Context, _ uint16, addr string) {
			results <- result{addr: addr, got: readUntil(ctx, addr, msg)}
		},
	}

	err := expect(w, 0, OK)(ctx, t)
	if err != nil {
		return err
	}

	select {
	case r := <-results:
		if !strings.HasPrefix(r.addr, "127.0.0.1:") || r.got != msg {
			return fmt.Errorf("published at %s, read %q; want 127.0.0.1 and %q", r.addr, r.got, msg)
		}

		return nil
	case <-ctx.Done():
		return errors.New("the target never reported the published address")
	}
}

// readUntil connects to addr until a connection returns want, which needs retries until the guest listens.
// It returns the last data read.
func readUntil(ctx context.Context, addr, want string) string {
	var got string

	for ctx.Err() == nil && got != want {
		var d net.Dialer

		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))

			data, _ := io.ReadAll(io.LimitReader(c, int64(len(want)))) // A short read means the guest is not ready.
			got = string(data)

			_ = c.Close()
		}

		if got != want {
			time.Sleep(200 * time.Millisecond)
		}
	}

	return got
}
