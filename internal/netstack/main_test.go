package netstack

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// TestMain fails the package when a test leaves goroutines or file descriptors of the network stack behind.
// The check runs after all tests, because parallel tests share the process.
func TestMain(m *testing.M) {
	// The network poller keeps its descriptors for the life of the process, so it opens before the baseline.
	if ln, err := net.Listen("tcp4", "127.0.0.1:0"); err == nil { //nolint:noctx // No test context exists yet.
		_ = ln.Close()
	}

	// The DHCP client of the guest simulator keeps /dev/urandom open after its first transaction ID.
	_, _ = dhcpv4.GenerateTransactionID()

	fds := openFDs()
	code := m.Run()

	// The fuzzing engine keeps descriptors open for its workers, so fuzzing runs skip the check.
	if fuzz := flag.Lookup("test.fuzz"); code == 0 && (fuzz == nil || fuzz.Value.String() == "") {
		if err := checkLeaks(fds); err != nil {
			fmt.Fprintln(os.Stderr, err)

			code = 1
		}
	}

	os.Exit(code)
}

// stackPackages identify goroutines that belong to a network stack.
var stackPackages = []string{"gvisor.dev/gvisor/", "gvisor-tap-vsock", "miekg/dns", "insomniacslk/dhcp", "sbxr/internal/netstack"}

func checkLeaks(fdsBefore int) error {
	var leaked []string

	for range 50 {
		// On Linux, io.Copy between TCP connections caches splice pipes in a sync.Pool until a GC frees them.
		runtime.GC()

		leaked = stackGoroutines()
		if len(leaked) == 0 && openFDs() <= fdsBefore {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	if len(leaked) > 0 {
		return fmt.Errorf("leaked %d goroutines:\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
	}

	return fmt.Errorf("leaked file descriptors: %d before, %d after", fdsBefore, openFDs())
}

func stackGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}

		buf = make([]byte, 2*len(buf))
	}

	// The first goroutine is the caller.
	var leaked []string

	for _, g := range strings.Split(string(buf), "\n\n")[1:] {
		for _, pkg := range stackPackages {
			if strings.Contains(g, pkg) {
				leaked = append(leaked, g)
				break
			}
		}
	}

	return leaked
}

func openFDs() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		panic(err)
	}

	return len(entries)
}

// TestCloseAtCaps holds the default caps, then checks that the network still closes quickly, so a guest at the caps cannot keep sbxr from exiting.
func TestCloseAtCaps(t *testing.T) {
	t.Parallel()

	d := newTestDialer(t)
	n, g := startNetwork(t, Config{Dial: d.dial, Publish: []Publish{{Guest: 80}}})

	ln, err := g.ListenTCP(80)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// The guest service never answers, so the published connection stays open and holds one TCP slot.
	accepted := make(chan struct{})

	go serve(ln, func(c net.Conn) {
		close(accepted)

		_, _ = c.Read(make([]byte, 1))
	})

	var nd net.Dialer

	pub := n.Published()[0].Host.String()

	host, err := nd.DialContext(t.Context(), "tcp", pub)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	<-accepted

	dst := netip.AddrPortFrom(publicAddr, 80).String()

	for range defaultMaxTCP - 1 {
		c, err := dialTCP(t, g, dst, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		if err := roundTrip(c, "held", 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := dialTCP(t, g, dst, 5*time.Second); err == nil {
		t.Fatal("TCP connection past the default cap works")
	}

	for range defaultMaxUDP {
		if err := roundTrip(dialUDP(t, g, netip.AddrPortFrom(publicAddr, 53).String()), "held", 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()

	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Small CI runners with the race detector need about 2 s, so the bound is the Close timeout, not the local time.
	if elapsed := time.Since(start); elapsed > closeTimeout {
		t.Errorf("Close took %v with held flows", elapsed)
	}

	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := host.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("published connection still open after Close")
	}

	var lc net.ListenConfig

	again, err := lc.Listen(t.Context(), "tcp4", pub)
	if err != nil {
		t.Fatalf("publish address still bound after Close: %v", err)
	}

	_ = again.Close()
}

// TestBlockedFramesNeverReachHost writes raw guest frames, so it also covers destinations that a guest stack refuses to send to.
func TestBlockedFramesNeverReachHost(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	port := netip.MustParseAddrPort(pc.LocalAddr().String()).Port()
	d := &testDialer{udp: pc.LocalAddr().String(), routed: []netip.Addr{publicAddr}}

	n, err := Start(t.Context(), Config{Dial: d.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	vm, err := net.FileConn(n.VMFile())
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()

	dsts := []netip.Addr{netip.MustParseAddr("192.168.127.255"), limitedBroadcast}

	for _, c := range egressCases {
		if c.addr != hostCase {
			dsts = append(dsts, netip.MustParseAddr(c.addr))
		}
	}

	for _, dst := range dsts {
		frame := ipv4Frame(dst, protoUDP, 40000, port, []byte("blocked "+dst.String()))
		if _, err := vm.Write(frame); err != nil {
			t.Fatal(err)
		}
	}

	// The switch handles frames in order, so the canary arrives after the stack has seen every blocked frame.
	if _, err := vm.Write(ipv4Frame(publicAddr, protoUDP, 40001, port, []byte("canary"))); err != nil {
		t.Fatal(err)
	}

	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)

	nr, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(buf[:nr]); got != "canary" {
		t.Errorf("host received %q, want only the canary", got)
	}

	_ = pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if nr, _, err := pc.ReadFrom(buf); err == nil {
		t.Errorf("host received %q after the canary", buf[:nr])
	}

	if dials := d.Dials(); len(dials) != 1 {
		t.Errorf("host dials = %v, want only the canary", dials)
	}
}

func TestSpoofedFramesDoNotGrowSwitch(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	d := &testDialer{udp: pc.LocalAddr().String(), routed: []netip.Addr{publicAddr}}

	n, err := Start(t.Context(), Config{Dial: d.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	vm, err := net.FileConn(n.VMFile())
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()

	for i := range 10_000 {
		mac := tcpip.LinkAddress([]byte{0x02, 0, 0, byte(i >> 16), byte(i >> 8), byte(i)})
		writeFrame(t, vm, arpFrame(mac, mac, guestIP))
	}

	port := netip.MustParseAddrPort(pc.LocalAddr().String()).Port()
	writeFrame(t, vm, ipv4Frame(publicAddr, protoUDP, 40001, port, []byte("canary")))

	_ = pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := pc.ReadFrom(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	if cam := n.sw.CAM(); len(cam) != 1 {
		t.Errorf("switch learned %d MAC addresses, want only the guest", len(cam))
	}
}

// writeFrame writes one frame and retries while the socket queue is full, which macOS reports as ENOBUFS.
func writeFrame(t *testing.T, c net.Conn, frame []byte) {
	t.Helper()

	for {
		_, err := c.Write(frame)
		if !errors.Is(err, syscall.ENOBUFS) {
			if err != nil {
				t.Fatal(err)
			}

			return
		}

		time.Sleep(time.Millisecond)
	}
}
