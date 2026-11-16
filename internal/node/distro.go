package node

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/albertsko/sbxr/internal/subid"
)

// distro is a node distribution that runs containerd under its own systemd unit.
type distro struct {
	name   string
	units  []string // Any active unit selects the distribution. Restarting it restarts containerd.
	config string   // Main containerd config, written by the distribution. The installer never edits it.
	dropIn string   // Directory that the main config imports.
	socket string   // containerd's gRPC socket, which serves the CRI.
}

// distros lists the supported distributions. containerd comes last, because k3s or RKE2 nodes may also have its unit.
var distros = []distro{
	{
		name:   "k3s",
		units:  []string{"k3s.service", "k3s-agent.service"},
		config: "/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		dropIn: "/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d",
		socket: "/run/k3s/containerd/containerd.sock",
	},
	{
		name:   "rke2",
		units:  []string{"rke2-server.service", "rke2-agent.service"},
		config: "/var/lib/rancher/rke2/agent/etc/containerd/config.toml",
		dropIn: "/var/lib/rancher/rke2/agent/etc/containerd/config-v3.toml.d",
		socket: "/run/k3s/containerd/containerd.sock",
	},
	{
		name:   "k0s",
		units:  []string{"k0sworker.service", "k0scontroller.service"},
		config: "/etc/k0s/containerd.toml",
		dropIn: "/etc/k0s/containerd.d",
		socket: "/run/k0s/containerd.sock",
	},
	{
		name:   "containerd",
		units:  []string{"containerd.service"},
		config: "/etc/containerd/config.toml",
		dropIn: "/etc/containerd/conf.d",
		socket: "/run/containerd/containerd.sock",
	},
}

// dropInFile is the path of the sbxr drop-in for d.
func (d distro) dropInFile() string {
	return d.dropIn + "/sbxr.toml"
}

// detect returns the first distribution with an active unit, and that unit.
func detect(active []string) (distro, string, error) {
	for _, d := range distros {
		for _, u := range d.units {
			if slices.Contains(active, u) {
				return d, u, nil
			}
		}
	}

	return distro{}, "", errors.New("no supported containerd unit is active (containerd, k3s, rke2, k0s)")
}

// facts is what preflight needs to know about a node.
type facts struct {
	kvm     bool
	glibc   string // Output of "getconf GNU_LIBC_VERSION", for example "glibc 2.39".
	runtime string // Node containerRuntimeVersion, for example "containerd://2.2.1".
	config  string // Main containerd config.
	systemd bool   // The node's runc runtime uses systemd cgroups.
	dbus    bool   // The D-Bus system socket exists.
	kernel  string // Output of "uname -r", for example "6.8.0-1015-azure".
	tun     bool   // /dev/net/tun exists.
	// subuid and subgid are /etc/subuid and /etc/subgid.
	subuid, subgid string
	// kubeletUser reports whether the kubelet user exists. kubeletUIDs and kubeletGIDs are the output of
	// "getsubids kubelet" and "getsubids -g kubelet", empty when getsubids failed. The kubelet reads its range the same way.
	kubeletUser              bool
	kubeletUIDs, kubeletGIDs string
	maxPods                  int // The node's pod capacity.
}

var (
	minGlibc      = version{2, 35}
	minContainerd = version{2, 2}
	// Phase 2 mounts overlays on idmapped lower layers, which Linux supports since 5.19.
	minKernel = version{5, 19}
)

// podIDs returns the kubelet's id range, and checks that a range for phase 2 pod VMs fits next to it.
//
// Without a kubelet entry, the kubelet gives pods with user namespaces (hostUsers: false) random ids from almost the
// whole id space, so any range could overlap them. With one, it uses only that entry, and the sbxr range stays apart.
func podIDs(f facts) (subid.Range, error) {
	kubelet, err := kubeletRange(f)
	if err != nil {
		return subid.Range{}, fmt.Errorf("kubelet id range: %w; the kubelet may give pods with user namespaces "+
			"any host id, which can overlap sbxr pod VMs (see the requirements in docs/kubernetes.md)", err)
	}

	// The installer chooses again under shadow's locks. This only rejects a node early.
	_, _, err = subid.Choose(f.subuid, f.subgid, kubelet)
	if err != nil {
		return subid.Range{}, fmt.Errorf("pod VM ids: %w", err)
	}

	return kubelet, nil
}

// kubeletRange returns the range that the kubelet uses, and checks it against the kubelet's own rules for the default
// 65536 ids per pod: the first id is a multiple of 65536 and at least 65536, and the length is a multiple of 65536
// that covers maxPods pods.
func kubeletRange(f facts) (subid.Range, error) {
	if !f.kubeletUser {
		return subid.Range{}, errors.New("the kubelet user does not exist")
	}

	uids, errU := subid.ParseGetsubids(f.kubeletUIDs)
	gids, errG := subid.ParseGetsubids(f.kubeletGIDs)

	switch {
	case errU != nil || errG != nil:
		return subid.Range{}, fmt.Errorf("getsubids kubelet: %w", errors.Join(errU, errG))
	case uids != gids:
		return subid.Range{}, fmt.Errorf("the kubelet uid range %s and gid range %s differ", uids, gids)
	case uids.First < subid.PerPod || uids.First%subid.PerPod != 0 || uids.Count%subid.PerPod != 0:
		return subid.Range{}, fmt.Errorf("the kubelet range %s does not start and end on a multiple of %d above the host ids", uids, subid.PerPod)
	case f.maxPods <= 0:
		return subid.Range{}, fmt.Errorf("the node's pod capacity %d is not valid", f.maxPods)
	case uids.Count/subid.PerPod < f.maxPods:
		return subid.Range{}, fmt.Errorf("the kubelet range %s holds %d pods, fewer than the node's %d", uids, uids.Count/subid.PerPod, f.maxPods)
	}

	return uids, nil
}

// preflight returns the drop-in for d and phase, or the reason why the node is not supported.
func preflight(d distro, f facts, phase int) ([]byte, error) {
	if !f.kvm {
		return nil, errors.New("/dev/kvm is missing")
	}

	glibc, ok := parseVersion(f.glibc)
	if !ok || glibc.less(minGlibc) {
		return nil, fmt.Errorf("glibc %q is older than %s", f.glibc, minGlibc)
	}

	containerd, ok := parseVersion(f.runtime)
	if !strings.HasPrefix(f.runtime, "containerd://") || !ok || containerd.less(minContainerd) {
		return nil, fmt.Errorf("container runtime %q is not containerd %s or later", f.runtime, minContainerd)
	}

	v := configVersion(f.config)
	if v != 2 && v != 3 {
		return nil, fmt.Errorf("%s has config version %d, want 2 or 3", d.config, v)
	}

	if !imports(f.config, d.config, d.dropInFile()) {
		return nil, fmt.Errorf("%s does not import %s/*.toml", d.config, d.dropIn)
	}

	if phase == 2 {
		kernel, ok := parseVersion(f.kernel)
		if !ok || kernel.less(minKernel) {
			return nil, fmt.Errorf("kernel %q is older than Linux %s", f.kernel, minKernel)
		}

		if !f.tun {
			return nil, errors.New("/dev/net/tun is missing")
		}

		return renderDropIn(v, f.systemd, phase), nil
	}

	if f.systemd && !f.dbus {
		return nil, errors.New("runc uses systemd cgroups, but /run/dbus/system_bus_socket is missing: install D-Bus")
	}

	return renderDropIn(v, f.systemd, phase), nil
}

// renderDropIn returns the drop-in for handler sbxr. Its version must match the main config.
// Phase 1 runs crun with libkrun through the runc shim. Phase 2 uses the sbxr shim as the pod sandboxer.
func renderDropIn(version int, systemd bool, phase int) []byte {
	plugin := "io.containerd.grpc.v1.cri"
	if version == 3 {
		plugin = "io.containerd.cri.v1.runtime"
	}

	if phase == 2 {
		return fmt.Appendf(nil, `# Written by the sbxr installer. sbxr-uninstall.yaml removes it.
version = %d

[plugins.%q.containerd.runtimes.sbxr]
  runtime_type = "io.containerd.sbxr.v1"
  runtime_path = "%s/bin/containerd-shim-sbxr-v1"
  sandboxer = "shim"
`, version, plugin, Prefix)
	}

	return fmt.Appendf(nil, `# Written by the sbxr installer. sbxr-uninstall.yaml removes it.
version = %d

[plugins.%q.containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"

[plugins.%q.containerd.runtimes.sbxr.options]
  BinaryName = "%s/bin/krun"
  Root = "%s"
  SystemdCgroup = %t
`, version, plugin, plugin, Prefix, RuntimeRoot, systemd)
}

var (
	versionLine   = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\d+)`)
	importsLine   = regexp.MustCompile(`(?ms)^\s*imports\s*=\s*\[(.*?)\]`)
	quoted        = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
	runcOptions   = regexp.MustCompile(`^\[\s*plugins\.\s*["']io\.containerd\.(grpc\.v1\.cri|cri\.v1\.runtime)["']\s*\.containerd\.runtimes\.'?"?runc"?'?\.options\s*\]$`)
	systemdCgroup = regexp.MustCompile(`^SystemdCgroup\s*=\s*(true|false)\b`)
	versionInStr  = regexp.MustCompile(`(\d+)\.(\d+)`)
)

// configVersion returns the top-level version of a containerd config, or 0 when it has none.
func configVersion(config string) int {
	m := versionLine.FindStringSubmatch(config)
	if m == nil {
		return 0
	}

	v, _ := strconv.Atoi(m[1])

	return v
}

// importPatterns returns the absolute import patterns of the config at path.
func importPatterns(config, path string) []string {
	m := importsLine.FindStringSubmatch(config)
	if m == nil {
		return nil
	}

	var patterns []string

	for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
		p := q[1] + q[2]
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}

		patterns = append(patterns, p)
	}

	return patterns
}

// imports reports whether the config at path imports file.
func imports(config, path, file string) bool {
	return slices.ContainsFunc(importPatterns(config, path), func(p string) bool {
		ok, _ := filepath.Match(p, file)

		return ok
	})
}

// runcSystemdCgroup returns the SystemdCgroup option of containerd's runc runtime, which the sbxr runtime must match.
// configs are in containerd's merge order (the main config, then each import), so the last setting wins.
// Other runtimes and the default (false) do not count.
func runcSystemdCgroup(configs ...string) bool {
	systemd := false

	for _, config := range configs {
		inRunc := false

		for line := range strings.Lines(config) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				inRunc = runcOptions.MatchString(line)

				continue
			}

			if m := systemdCgroup.FindStringSubmatch(line); inRunc && m != nil {
				systemd = m[1] == "true"
			}
		}
	}

	return systemd
}

type version struct{ major, minor int }

func (v version) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }

func (v version) less(w version) bool {
	return cmp.Or(cmp.Compare(v.major, w.major), cmp.Compare(v.minor, w.minor)) < 0
}

// parseVersion returns the first "major.minor" in s.
func parseVersion(s string) (version, bool) {
	m := versionInStr.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])

	return version{major, minor}, true
}
