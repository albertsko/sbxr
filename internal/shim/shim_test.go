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
		{"/dev/shm is a guest tmpfs", dests["/dev/shm"].Type == "tmpfs"},
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
