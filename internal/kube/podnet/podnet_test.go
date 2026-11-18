//go:build linux

package podnet

import (
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/kube/podvm"
)

func TestGuestRoutes(t *testing.T) {
	t.Parallel()

	cidr := func(s string) *net.IPNet {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}

		return n
	}

	// The routes of a ptp pod interface, as the kernel lists them.
	routes := []netlink.Route{
		{Gw: net.ParseIP("10.244.0.1"), Protocol: unix.RTPROT_BOOT},
		{Dst: cidr("10.244.0.0/24"), Gw: net.ParseIP("10.244.0.1"), Protocol: unix.RTPROT_BOOT},
		{Dst: cidr("10.244.0.1/32"), Protocol: unix.RTPROT_BOOT},
		{Dst: cidr("10.244.0.0/24"), Protocol: unix.RTPROT_KERNEL},
		{Dst: cidr("fe80::/64"), Protocol: unix.RTPROT_BOOT},
		{Gw: net.ParseIP("10.244.0.1").To16(), Flags: int(netlink.FLAG_ONLINK), Dst: cidr("192.168.0.0/16")},
	}

	gw := netip.MustParseAddr("10.244.0.1")
	want := []podvm.Route{
		{Dst: netip.MustParsePrefix("10.244.0.1/32")},
		{Gateway: gw},
		{Dst: netip.MustParsePrefix("10.244.0.0/24"), Gateway: gw},
		{Dst: netip.MustParsePrefix("192.168.0.0/16"), Gateway: gw, OnLink: true},
	}

	if got := guestRoutes(routes); !slices.Equal(got, want) {
		t.Errorf("guestRoutes =\n%+v\nwant on-link routes first, without kernel and link-local routes:\n%+v", got, want)
	}
}
