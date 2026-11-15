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
)

// distro is a node distribution that runs containerd under its own systemd unit.
type distro struct {
	name   string
	units  []string // Any active unit selects the distribution. Restarting it restarts containerd.
	config string   // Main containerd config, written by the distribution. The installer never edits it.
	dropIn string   // Directory that the main config imports.
}

// distros lists the supported distributions. containerd comes last, because k3s or RKE2 nodes may also have its unit.
var distros = []distro{
	{
		name:   "k3s",
		units:  []string{"k3s.service", "k3s-agent.service"},
		config: "/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
		dropIn: "/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d",
	},
	{
		name:   "rke2",
		units:  []string{"rke2-server.service", "rke2-agent.service"},
		config: "/var/lib/rancher/rke2/agent/etc/containerd/config.toml",
		dropIn: "/var/lib/rancher/rke2/agent/etc/containerd/config-v3.toml.d",
	},
	{
		name:   "k0s",
		units:  []string{"k0sworker.service", "k0scontroller.service"},
		config: "/etc/k0s/containerd.toml",
		dropIn: "/etc/k0s/containerd.d",
	},
	{
		name:   "containerd",
		units:  []string{"containerd.service"},
		config: "/etc/containerd/config.toml",
		dropIn: "/etc/containerd/conf.d",
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
}

var (
	minGlibc      = version{2, 35}
	minContainerd = version{2, 2}
)

// preflight returns the drop-in for d, or the reason why the node is not supported.
func preflight(d distro, f facts) ([]byte, error) {
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

	if f.systemd && !f.dbus {
		return nil, errors.New("runc uses systemd cgroups, but /run/dbus/system_bus_socket is missing: install D-Bus")
	}

	return renderDropIn(v, f.systemd), nil
}

// renderDropIn returns the drop-in for handler sbxr. Its version must match the main config.
func renderDropIn(version int, systemd bool) []byte {
	plugin := "io.containerd.grpc.v1.cri"
	if version == 3 {
		plugin = "io.containerd.cri.v1.runtime"
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
