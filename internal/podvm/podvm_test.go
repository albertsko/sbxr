package podvm_test

import (
	"bufio"
	"net/netip"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/podvm"
)

func TestHeaderRoundTrip(t *testing.T) {
	t.Parallel()

	for _, h := range []podvm.Header{
		{Kind: podvm.Stdout, Container: "c1", Exec: "", Port: 0},
		{Kind: podvm.Stdin, Container: "c1", Exec: "probe-1", Port: 0},
		{Kind: podvm.PortForward, Container: "", Exec: "", Port: 8080},
	} {
		got, err := podvm.ReadHeader(bufio.NewReader(strings.NewReader(h.String() + "\npayload")))
		if err != nil || got != h {
			t.Errorf("ReadHeader(%q) = %+v, %v; want %+v", h.String(), got, err, h)
		}
	}

	for _, bad := range []string{"stdout c1\n", "port x\n", "exec c1 -\n", "stdout c1 -"} {
		if _, err := podvm.ReadHeader(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Errorf("ReadHeader(%q) succeeded", bad)
		}
	}
}

func TestPodRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := podvm.Pod{
		Hostname:  "web",
		Addresses: []netip.Prefix{netip.MustParsePrefix("10.244.0.5/24")},
		Routes:    []podvm.Route{{Dst: netip.Prefix{}, Gateway: netip.MustParseAddr("10.244.0.1"), OnLink: true}},
		MTU:       1500,
	}

	if err := podvm.WritePod(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := podvm.ReadPod(dir)
	if err != nil || got.Hostname != want.Hostname || got.Addresses[0] != want.Addresses[0] || got.Routes[0] != want.Routes[0] {
		t.Errorf("ReadPod = %+v, %v; want %+v", got, err, want)
	}
}
