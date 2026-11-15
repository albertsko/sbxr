//go:build linux

package shim

import (
	"path"
	"slices"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/albertsko/sbxr/internal/podvm"
)

// share is a host path that the shim mounts into the container's bundle in the pod share, at Name.
type share struct {
	Source   string
	Name     string
	ReadOnly bool
}

// guestSpec rewrites containerd's container spec for crun in the guest, where the bundle is guestBundle.
// It returns the host sources of the spec's bind mounts, which the shim shares into the bundle.
//
// The containers of a pod share the guest's network, IPC, and UTS namespaces, and so the pod's address and hostname.
// Each container keeps its own PID, mount, and cgroup namespaces. Host-only settings such as hooks and LSM labels go.
func guestSpec(s *specs.Spec, id, guestBundle string) []share {
	s.Root.Path = "rootfs"
	s.Hooks = nil
	s.Hostname = ""

	var shares []share

	for i := range s.Mounts {
		m := &s.Mounts[i]

		// The containers of a pod share its IPC namespace, so they share one /dev/shm: the pod's tmpfs in the guest.
		// A volume that the pod mounts at /dev/shm stays a volume.
		if isPodShm(*m) {
			*m = specs.Mount{Destination: devShm, Type: bindType, Source: podvm.GuestShm, Options: []string{"rbind", "nosuid", "nodev", "noexec"}}

			continue
		}

		if !isBind(*m) {
			continue
		}

		name := "mounts/" + strconv.Itoa(len(shares))
		shares = append(shares, share{Source: m.Source, Name: name, ReadOnly: slices.Contains(m.Options, "ro")})
		m.Source = path.Join(guestBundle, name)
	}

	// containerd mounts /etc/resolv.conf and /etc/hostname only from files that its own sandbox controller writes.
	// The shim writes them into the pod share instead.
	for _, f := range []string{"/etc/resolv.conf", "/etc/hostname"} {
		if !slices.ContainsFunc(s.Mounts, func(m specs.Mount) bool { return m.Destination == f }) {
			s.Mounts = append(s.Mounts, specs.Mount{Destination: f, Type: bindType, Source: path.Join(podvm.GuestShare, path.Base(f)), Options: []string{"rbind", "ro"}})
		}
	}

	if s.Process != nil {
		s.Process.ApparmorProfile = ""
		s.Process.SelinuxLabel = ""
	}

	if s.Linux == nil {
		return shares
	}

	s.Linux.CgroupsPath = "/sbxr/" + id
	s.Linux.MountLabel = ""
	s.Linux.IntelRdt = nil
	s.Linux.Namespaces = slices.DeleteFunc(s.Linux.Namespaces, func(ns specs.LinuxNamespace) bool {
		return ns.Type == specs.NetworkNamespace || ns.Type == specs.IPCNamespace || ns.Type == specs.UTSNamespace || ns.Type == specs.UserNamespace
	})

	// A path names a host namespace, such as the pod's shared PID namespace. In the guest the container gets a new one.
	for i := range s.Linux.Namespaces {
		s.Linux.Namespaces[i].Path = ""
	}

	return shares
}

const (
	bindType = "bind"
	devShm   = "/dev/shm"
)

// isPodShm reports whether m is a container's own /dev/shm: the tmpfs of containerd's default spec, or the bind of the
// shm that containerd's own sandbox controller creates under its sandboxes directory.
func isPodShm(m specs.Mount) bool {
	if m.Destination != devShm {
		return false
	}

	return m.Type == "tmpfs" || (isBind(m) && strings.Contains(m.Source, "/sandboxes/") && path.Base(m.Source) == "shm")
}

func isBind(m specs.Mount) bool {
	return m.Type == bindType || slices.Contains(m.Options, bindType) || slices.Contains(m.Options, "rbind")
}
