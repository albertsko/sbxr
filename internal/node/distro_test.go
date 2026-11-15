package node

import (
	"strings"
	"testing"
)

const kindConfig = `version = 2
imports = ["/etc/containerd/conf.d/*.toml"]
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
  SystemdCgroup = true
`

func TestPreflight(t *testing.T) {
	t.Parallel()

	containerd := distros[len(distros)-1]
	k3s := distros[0]
	ok := facts{kvm: true, glibc: "glibc 2.39", runtime: "containerd://2.3.4", config: kindConfig, systemd: true, dbus: true, kernel: "6.8.0-1015-azure", tun: true}

	tests := []struct {
		name   string
		distro distro
		phase  int // 1 when zero.
		change func(*facts)
		reason string // Empty when preflight passes.
	}{
		{name: "kind", distro: containerd, change: func(*facts) {}},
		{name: "k3s", distro: k3s, change: func(f *facts) {
			f.runtime = "containerd://v2.2.10-k3s1"
			f.config = "version = 3\nimports = [\"/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/*.toml\"]\n"
		}},
		{name: "relative import", distro: containerd, change: func(f *facts) { f.config = "version = 3\nimports = ['conf.d/*.toml']" }},
		{name: "multi-line imports", distro: containerd, change: func(f *facts) {
			f.config = "version = 3\nimports = [\n  \"/etc/a.toml\",\n  \"/etc/containerd/conf.d/*.toml\",\n]"
		}},
		{name: "no kvm", distro: containerd, change: func(f *facts) { f.kvm = false }, reason: "/dev/kvm is missing"},
		{name: "old glibc", distro: containerd, change: func(f *facts) { f.glibc = "glibc 2.31" }, reason: `glibc "glibc 2.31" is older than 2.35`},
		{name: "no glibc", distro: containerd, change: func(f *facts) { f.glibc = "" }, reason: "older than 2.35"},
		{name: "old containerd", distro: containerd, change: func(f *facts) { f.runtime = "containerd://1.7.24" }, reason: "not containerd 2.2 or later"},
		{name: "cri-o", distro: containerd, change: func(f *facts) { f.runtime = "cri-o://1.34.0" }, reason: "not containerd 2.2 or later"},
		{name: "no version", distro: containerd, change: func(f *facts) { f.config = "imports = [\"/etc/containerd/conf.d/*.toml\"]" }, reason: "config version 0, want 2 or 3"},
		{name: "no imports", distro: containerd, change: func(f *facts) { f.config = "version = 2\n" }, reason: "does not import /etc/containerd/conf.d/*.toml"},
		{name: "other imports", distro: containerd, change: func(f *facts) { f.config = "version = 2\nimports = [\"/etc/other/*.toml\"]" }, reason: "does not import"},
		{name: "systemd without dbus", distro: containerd, change: func(f *facts) { f.dbus = false }, reason: "install D-Bus"},
		{name: "cgroupfs without dbus", distro: containerd, change: func(f *facts) { f.systemd, f.dbus = false, false }},
		{name: "phase 2", distro: containerd, phase: 2, change: func(*facts) {}},
		{name: "phase 2 without dbus", distro: containerd, phase: 2, change: func(f *facts) { f.dbus = false }},
		{name: "phase 2 on old Linux", distro: containerd, phase: 2, change: func(f *facts) { f.kernel = "5.15.0-91-generic" }, reason: `kernel "5.15.0-91-generic" is older than Linux 5.19`},
		{name: "phase 2 without TUN", distro: containerd, phase: 2, change: func(f *facts) { f.tun = false }, reason: "/dev/net/tun is missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := ok
			tt.change(&f)

			dropIn, err := preflight(tt.distro, f, max(tt.phase, 1))
			if tt.reason == "" {
				if err != nil {
					t.Fatalf("preflight failed: %v", err)
				}

				if configVersion(string(dropIn)) != configVersion(f.config) {
					t.Errorf("drop-in version differs from the main config:\n%s", dropIn)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("error %v, want it to contain %q", err, tt.reason)
			}
		})
	}
}

func TestRenderDropIn(t *testing.T) {
	t.Parallel()

	phase1 := []string{`BinaryName = "/opt/sbxr/bin/krun"`, `runtime_type = "io.containerd.runc.v2"`}
	phase2 := []string{`runtime_type = "io.containerd.sbxr.v1"`, `runtime_path = "/opt/sbxr/bin/containerd-shim-sbxr-v1"`, `sandboxer = "shim"`}

	tests := []struct {
		version int
		systemd bool
		phase   int
		want    []string
	}{
		{2, true, 1, append([]string{"version = 2", `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr]`, "SystemdCgroup = true"}, phase1...)},
		{3, false, 1, append([]string{"version = 3", `[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr.options]`, "SystemdCgroup = false"}, phase1...)},
		{2, true, 2, append([]string{"version = 2", `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.sbxr]`}, phase2...)},
		{3, true, 2, append([]string{"version = 3", `[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr]`}, phase2...)},
	}

	for _, tt := range tests {
		got := string(renderDropIn(tt.version, tt.systemd, tt.phase))
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("drop-in v%d lacks %q:\n%s", tt.version, w, got)
			}
		}
	}
}

func TestDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		active []string
		want   string // Unit, or empty when nothing is detected.
	}{
		{[]string{"dbus.service", "containerd.service", "kubelet.service"}, "containerd.service"},
		{[]string{"containerd.service", "k3s-agent.service"}, "k3s-agent.service"},
		{[]string{"rke2-server.service"}, "rke2-server.service"},
		{[]string{"k0sworker.service"}, "k0sworker.service"},
		{[]string{"docker.service", "crio.service"}, ""},
	}

	for _, tt := range tests {
		_, unit, err := detect(tt.active)
		if unit != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("detect(%v) = %q, %v; want %q", tt.active, unit, err, tt.want)
		}
	}
}

func TestRuncSystemdCgroup(t *testing.T) {
	t.Parallel()

	const (
		v2Runc   = "[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc.options]\n"
		v3Runc   = "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]\n"
		v2Nvidia = "[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.nvidia.options]\n"
	)

	tests := []struct {
		name    string
		configs []string
		want    bool
	}{
		{"kind", []string{kindConfig}, true},
		{"v3", []string{"version = 3\n" + v3Runc + "  SystemdCgroup = true\n"}, true},
		{"unset", []string{"version = 2\n" + v2Runc + "  BinaryName = \"runc\"\n"}, false},
		{"other runtime only", []string{v2Runc + "  SystemdCgroup = false\n" + v2Nvidia + "  SystemdCgroup = true\n"}, false},
		{"runc after other runtime", []string{v2Nvidia + "  SystemdCgroup = false\n" + v2Runc + "  SystemdCgroup = true\n"}, true},
		{"import overrides main", []string{v2Runc + "  SystemdCgroup = true\n", v2Runc + "  SystemdCgroup = false\n"}, false},
		{"import without runc keeps main", []string{v2Runc + "  SystemdCgroup = true\n", v2Nvidia + "  SystemdCgroup = false\n"}, true},
	}

	for _, tt := range tests {
		if got := runcSystemdCgroup(tt.configs...); got != tt.want {
			t.Errorf("%s: %t, want %t", tt.name, got, tt.want)
		}
	}
}
