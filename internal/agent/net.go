//go:build linux

package agent

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/podvm"
)

// podInterface is the guest's only network device. libkrun gives it the pod interface's MAC address.
const podInterface = "eth0"

// setupPod sets the hostname and gives the pod interface's addresses and routes to the guest interface.
func setupPod(p podvm.Pod) error {
	if p.Hostname != "" {
		if err := unix.Sethostname([]byte(p.Hostname)); err != nil {
			return fmt.Errorf("set hostname: %w", err)
		}
	}

	lo, err := netlink.LinkByName("lo")
	if err == nil {
		err = netlink.LinkSetUp(lo)
	}

	if err != nil {
		return fmt.Errorf("set up loopback: %w", err)
	}

	link, err := netlink.LinkByName(podInterface)
	if err != nil {
		return fmt.Errorf("find %s: %w", podInterface, err)
	}

	if p.MTU > 0 {
		if err := netlink.LinkSetMTU(link, p.MTU); err != nil {
			return fmt.Errorf("set MTU: %w", err)
		}
	}

	for _, a := range p.Addresses {
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: ipNet(a)}); err != nil {
			return fmt.Errorf("add address %s: %w", a, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set up %s: %w", podInterface, err)
	}

	for _, r := range p.Routes {
		route := &netlink.Route{LinkIndex: link.Attrs().Index}
		if r.Dst.IsValid() {
			route.Dst = ipNet(r.Dst)
		}

		if r.Gateway.IsValid() {
			route.Gw = net.IP(r.Gateway.AsSlice())
		}

		if r.OnLink {
			route.Flags = int(netlink.FLAG_ONLINK)
		}

		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("add route %+v: %w", r, err)
		}
	}

	return nil
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: net.IP(p.Addr().AsSlice()), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}
