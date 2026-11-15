//go:build linux

// Package podnet moves a pod's network to a TAP device for its VM (PRD "Kubernetes phase 2", pod networking).
//
// In the pod network namespace, [Setup] creates a TAP device owned by the VM process's uid, redirects all traffic
// both ways between the pod interface and the TAP device, and moves the pod interface's addresses and routes to the guest.
package podnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/podvm"
)

// Names of the devices in the pod network namespace.
const (
	PodInterface = "eth0"
	TAP          = "sbxr0"
)

// Guest is what the guest needs from the pod interface.
type Guest struct {
	MAC       net.HardwareAddr
	Addresses []netip.Prefix
	Routes    []podvm.Route
	MTU       int
}

// Setup runs in the network namespace at nsPath and creates the TAP device for uid and gid.
// The pod interface keeps its MAC address but loses its addresses and routes, which the guest takes over.
func Setup(nsPath string, uid, gid int) (Guest, error) {
	var g Guest

	err := Do(nsPath, func() error {
		var err error

		g, err = setup(uid, gid)

		return err
	})

	return g, err
}

func setup(uid, gid int) (Guest, error) {
	var g Guest

	pod, err := netlink.LinkByName(PodInterface)
	if err != nil {
		return g, fmt.Errorf("find %s: %w", PodInterface, err)
	}

	g.MAC = pod.Attrs().HardwareAddr
	g.MTU = pod.Attrs().MTU

	addrs, err := netlink.AddrList(pod, netlink.FAMILY_ALL)
	if err != nil {
		return g, fmt.Errorf("list addresses: %w", err)
	}

	for _, a := range addrs {
		if a.IP.IsLinkLocalUnicast() {
			continue
		}

		p, ok := prefix(a.IPNet)
		if ok {
			g.Addresses = append(g.Addresses, p)
		}
	}

	routes, err := netlink.RouteList(pod, netlink.FAMILY_ALL)
	if err != nil {
		return g, fmt.Errorf("list routes: %w", err)
	}

	g.Routes = guestRoutes(routes)

	tap := &netlink.Tuntap{
		Name: TAP, MTU: g.MTU,
		Mode:  netlink.TUNTAP_MODE_TAP,
		Flags: netlink.TUNTAP_NO_PI,
		Owner: uint32(uid), //nolint:gosec // Ids from the pod range fit.
		Group: uint32(gid), //nolint:gosec // Ids from the pod range fit.
	}

	err = netlink.LinkAdd(tap)

	// The device persists. The VM process opens it again, so the queues that LinkAdd opened go.
	for _, f := range tap.Fds {
		_ = f.Close()
	}

	if err == nil {
		err = netlink.LinkSetUp(tap)
	}

	if err != nil {
		return g, fmt.Errorf("create TAP device %s: %w", TAP, err)
	}

	for _, pair := range [][2]netlink.Link{{pod, tap}, {tap, pod}} {
		if err := redirect(pair[0], pair[1]); err != nil {
			return g, err
		}
	}

	// Without addresses in this namespace, the kernel no longer answers for the pod address. The guest does.
	for _, a := range addrs {
		if err := netlink.AddrDel(pod, &a); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
			return g, fmt.Errorf("remove address %s: %w", a.IPNet, err)
		}
	}

	return g, nil
}

// guestRoutes returns the routes of the pod interface for the guest: on-link and covered routes first, then routes through gateways.
func guestRoutes(routes []netlink.Route) []podvm.Route {
	var out []podvm.Route

	for _, r := range routes {
		if r.Protocol == unix.RTPROT_KERNEL || (r.Dst != nil && r.Dst.IP.IsLinkLocalUnicast()) {
			continue // The guest kernel adds the routes of its addresses itself.
		}

		var gr podvm.Route

		if r.Dst != nil {
			gr.Dst, _ = prefix(r.Dst)
		}

		if r.Gw != nil {
			gr.Gateway, _ = netip.AddrFromSlice(r.Gw)
			gr.Gateway = gr.Gateway.Unmap()
		}

		gr.OnLink = r.Flags&int(netlink.FLAG_ONLINK) != 0
		out = append(out, gr)
	}

	slices.SortStableFunc(out, func(a, b podvm.Route) int {
		return boolInt(a.Gateway.IsValid()) - boolInt(b.Gateway.IsValid())
	})

	return out
}

// redirect sends every frame that arrives on from out of to.
func redirect(from, to netlink.Link) error {
	qdisc := &netlink.Ingress{
		LinkIndex: from.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_INGRESS,
	}

	err := netlink.QdiscAdd(qdisc)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add ingress qdisc to %s: %w", from.Attrs().Name, err)
	}

	filter := &netlink.U32{
		LinkIndex: from.Attrs().Index,
		Parent:    netlink.MakeHandle(0xffff, 0),
		Priority:  1,
		Protocol:  unix.ETH_P_ALL,
		Actions: []netlink.Action{&netlink.MirredAction{
			Action:       netlink.TC_ACT_STOLEN,
			MirredAction: netlink.TCA_EGRESS_REDIR,
			Ifindex:      to.Attrs().Index,
		}},
	}

	err = netlink.FilterAdd(filter)
	if err != nil {
		return fmt.Errorf("redirect %s to %s: %w", from.Attrs().Name, to.Attrs().Name, err)
	}

	return nil
}

// Do runs fn on a thread in the network namespace at path. The thread is discarded afterwards.
func Do(path string, fn func() error) error {
	errc := make(chan error, 1)

	go func() {
		// The thread never returns to the pool, because it stays in the pod namespace.
		runtime.LockOSThread()

		ns, err := netns.GetFromPath(path)
		if err != nil {
			errc <- fmt.Errorf("open network namespace %s: %w", path, err)

			return
		}
		defer ns.Close() //nolint:errcheck // Read-only handle.

		err = netns.Set(ns)
		if err != nil {
			errc <- fmt.Errorf("enter network namespace %s: %w", path, err)

			return
		}

		errc <- fn()
	}()

	return <-errc
}

func prefix(n *net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}

	ones, _ := n.Mask.Size()

	return netip.PrefixFrom(addr.Unmap(), ones), true
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}
