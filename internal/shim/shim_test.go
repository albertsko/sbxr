//go:build linux

package shim

import (
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestGuestSpec(t *testing.T) {
	t.Parallel()

	s := &specs.Spec{
		Root:     &specs.Root{Path: "/run/containerd/rootfs", Readonly: false},
		Hostname: "web",
		Hooks:    &specs.Hooks{},
		Process:  &specs.Process{ApparmorProfile: "cri-containerd.apparmor.d", Args: []string{"sh"}},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/etc/hosts", Type: "bind", Source: "/var/lib/kubelet/pods/1/etc-hosts", Options: []string{"rbind", "rw"}},
			{Destination: "/data", Source: "/var/lib/kubelet/pods/1/volumes/x", Options: []string{"rbind", "ro"}},
			{Destination: "/dev/shm", Type: "bind", Source: "/run/containerd/sandboxes/1/shm", Options: []string{"rbind"}},
		},
		Linux: &specs.Linux{
			CgroupsPath: "kubepods-pod1.slice:cri-containerd:c1",
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace, Path: "/proc/42/ns/pid"},
				{Type: specs.IPCNamespace, Path: "/proc/42/ns/ipc"},
				{Type: specs.UTSNamespace, Path: "/proc/42/ns/uts"},
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace, Path: "/var/run/netns/cni-1"},
			},
		},
	}

	shares := guestSpec(s, "c1", "/run/sbxr/shared/c1")

	want := []share{
		{Source: "/var/lib/kubelet/pods/1/etc-hosts", Name: "mounts/0", ReadOnly: false},
		{Source: "/var/lib/kubelet/pods/1/volumes/x", Name: "mounts/1", ReadOnly: true},
	}
	if !slices.Equal(shares, want) {
		t.Errorf("shares %+v, want %+v", shares, want)
	}

	dests := map[string]specs.Mount{}
	for _, m := range s.Mounts {
		dests[m.Destination] = m
	}

	checks := []struct {
		name string
		ok   bool
	}{
		{"root is relative", s.Root.Path == "rootfs"},
		{"no hooks", s.Hooks == nil},
		{"no hostname, the pod's UTS namespace has it", s.Hostname == ""},
		{"no AppArmor profile", s.Process.ApparmorProfile == ""},
		{"volume source in the bundle", dests["/data"].Source == "/run/sbxr/shared/c1/mounts/1"},
		{"/dev/shm is the pod's shm", dests["/dev/shm"].Source == "/run/sbxr/shm" && dests["/dev/shm"].Type == "bind"},
		{"resolv.conf from the pod share", dests["/etc/resolv.conf"].Source == "/run/sbxr/shared/resolv.conf"},
		{"hostname from the pod share", dests["/etc/hostname"].Source == "/run/sbxr/shared/hostname"},
		{"guest cgroup", s.Linux.CgroupsPath == "/sbxr/c1"},
		{"new PID and mount namespaces only", slices.Equal(s.Linux.Namespaces, []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.MountNamespace}})},
	}

	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: spec %+v", c.name, s)
		}
	}
}

func TestParsePodConfig(t *testing.T) {
	t.Parallel()

	field := func(b []byte, num protowire.Number, v []byte) []byte {
		b = protowire.AppendTag(b, num, protowire.BytesType)

		return protowire.AppendBytes(b, v)
	}
	varint := func(b []byte, num protowire.Number, v uint64) []byte {
		b = protowire.AppendTag(b, num, protowire.VarintType)

		return protowire.AppendVarint(b, v)
	}

	resources := varint(varint(varint(nil, fieldMemory, 512<<20), fieldCPUQuota, 150000), fieldCPUPeriod, 100000)
	overhead := varint(nil, fieldMemory, 64<<20)
	linux := field(field(field(nil, fieldCgroupParent, []byte("kubepods-pod1.slice")), fieldResources, resources), 4, overhead)
	dns := field(field(field(nil, fieldDNSServers, []byte("10.96.0.10")), fieldDNSSearches, []byte("default.svc.cluster.local")), fieldDNSOptions, []byte("ndots:5"))
	msg := field(field(field(varint(nil, 1, 7), fieldHostname, []byte("web")), fieldDNS, dns), fieldLinux, linux)

	c, err := parsePodConfig(msg)
	if err != nil {
		t.Fatal(err)
	}

	if c.Hostname != "web" || c.CgroupParent != "kubepods-pod1.slice" || c.MemoryBytes != 512<<20 {
		t.Errorf("config %+v", c)
	}

	if got := vmMemoryMiB(c); got != 512 {
		t.Errorf("VM memory %d MiB, want 512", got)
	}

	if got, want := string(c.resolvConf()), "search default.svc.cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n"; got != want {
		t.Errorf("resolv.conf %q, want %q", got, want)
	}
}

func TestCgroupPath(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"kubepods-burstable-pod1_2.slice": "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1_2.slice",
		"/kubepods/burstable/pod1":        "/kubepods/burstable/pod1",
	}

	for in, want := range tests {
		if got := cgroupPath(in); got != want {
			t.Errorf("cgroupPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPodShm checks that the containers of a pod share one /dev/shm, as they share the pod's IPC namespace,
// while a volume that the pod mounts at /dev/shm stays that volume.
func TestPodShm(t *testing.T) {
	t.Parallel()

	shm := func(s *specs.Spec) specs.Mount {
		for _, m := range s.Mounts {
			if m.Destination == "/dev/shm" {
				return m
			}
		}

		return specs.Mount{}
	}

	// containerd's default spec gives each container its own tmpfs.
	defaultShm := specs.Mount{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}}

	sources := make([]string, 0, 2)

	for _, id := range []string{"web", "sidecar"} {
		s := &specs.Spec{Root: &specs.Root{Path: "/rootfs"}, Mounts: []specs.Mount{defaultShm}}
		guestSpec(s, id, "/run/sbxr/shared/"+id)
		sources = append(sources, shm(s).Source)
	}

	if sources[0] != "/run/sbxr/shm" || sources[1] != sources[0] {
		t.Errorf("/dev/shm sources %q, want both the pod's shm", sources)
	}

	volume := &specs.Spec{Root: &specs.Root{Path: "/rootfs"}, Mounts: []specs.Mount{
		{Destination: "/dev/shm", Type: "bind", Source: "/var/lib/kubelet/pods/1/volumes/kubernetes.io~empty-dir/shm", Options: []string{"rbind"}},
	}}

	shares := guestSpec(volume, "c1", "/run/sbxr/shared/c1")
	if len(shares) != 1 || shm(volume).Source != "/run/sbxr/shared/c1/mounts/0" {
		t.Errorf("shares %+v, /dev/shm %+v; want the volume shared", shares, shm(volume))
	}
}

func TestBundleOf(t *testing.T) {
	t.Parallel()

	for exe, want := range map[string]string{
		"/opt/sbxr/bin/containerd-shim-sbxr-v1":                               "/opt/sbxr",
		"/opt/sbxr.old/20261115T195118.213163000/bin/containerd-shim-sbxr-v1": "/opt/sbxr.old/20261115T195118.213163000",
		"/tmp/shim.test": "/opt/sbxr",
	} {
		if got := bundleOf(exe); got != want {
			t.Errorf("bundleOf(%q) = %q, want %q", exe, got, want)
		}
	}
}
