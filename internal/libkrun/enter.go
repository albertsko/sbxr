//go:build darwin || linux

package libkrun

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/term"
)

// Values from libkrun 1.19.8 include/libkrun.h.
const (
	maxLogLevel = 5

	// rootTag is KRUN_FS_ROOT_TAG, the virtio-fs tag of the root share.
	rootTag = "/dev/root"

	netFlagDHCPClient = 1 << 1

	netFeatureCsum      = 1 << 0
	netFeatureGuestCsum = 1 << 1
	netFeatureGuestTSO4 = 1 << 7
	netFeatureGuestUFO  = 1 << 10
	netFeatureHostTSO4  = 1 << 11
	netFeatureHostUFO   = 1 << 14

	compatNetFeatures = netFeatureCsum | netFeatureGuestCsum | netFeatureGuestTSO4 |
		netFeatureGuestUFO | netFeatureHostTSO4 | netFeatureHostUFO
)

// Config describes one VM.
type Config struct {
	// Root is the absolute host path of the guest root directory.
	Root string
	// CPUs is the vCPU count, at least 1.
	CPUs uint8
	// MemoryMiB is the guest RAM in MiB, at least 1.
	MemoryMiB uint32
	// Net adds a virtio-net device. Without it the guest has no network.
	Net *Net
	// Volumes adds one virtio-fs device per host directory.
	Volumes []Volume
	// MountHelper is the guest mount helper, which libkrun serves at MountHelperPath when there are volumes.
	MountHelper []byte
	// MountHelperPath is the absolute guest path of MountHelper.
	MountHelperPath string
	// TAP adds a virtio-net device on a TAP device that the VM process can open. It excludes Net.
	TAP *TAP
	// Shares are extra virtio-fs devices.
	Shares []Share
	// Vsock connects guest vsock ports to host Unix sockets. It adds a vsock device without socket forwarding.
	Vsock []VsockPort
	// LogLevel is the libkrun log level, from 0 (off) to 5 (trace).
	LogLevel uint32
}

// Volume is a virtio-fs device for a host directory. ReadOnly makes libkrun reject guest writes.
type Volume struct {
	Tag      string
	Host     string
	ReadOnly bool
}

// Net is a virtio-net device backed by a connected unixgram socket that the VM process inherited.
type Net struct {
	FD  int
	MAC [6]byte
}

// TAP is a virtio-net device on a TAP device in the network namespace of the VM process.
type TAP struct {
	Name string
	MAC  [6]byte
}

// Share is a host directory that the guest mounts with virtio-fs under Tag.
type Share struct {
	Tag  string
	Path string
}

// VsockPort pairs a guest vsock port with a Unix socket in the host.
type VsockPort struct {
	Port uint32
	Path string
	// Listen makes libkrun listen on Path and connect each host connection to a guest listener on Port.
	Listen bool
}

// api holds the libkrun functions that sbxr uses. Each one returns 0 or a negative errno.
// Do not add krun_set_exec, krun_set_workdir, or krun_set_rlimits: they put values on the kernel command line.
// The kernel command line breaks on quotes and non-ASCII text, and its values override /.krun_config.json.
type api struct {
	setLogLevel          func(level uint32) int32
	createCtx            func() int32
	setVMConfig          func(ctx uint32, cpus uint8, ramMiB uint32) int32
	setRoot              func(ctx uint32, path string) int32
	setEnv               func(ctx uint32, envp unsafe.Pointer) int32
	addNetUnixgram       func(ctx uint32, path unsafe.Pointer, fd int32, mac *byte, features, flags uint32) int32
	addVirtiofs4         func(ctx uint32, tag, path string, shmSize uint64, readOnly bool, semantics uint32) int32
	addOverlayFile       func(ctx uint32, tag, path string, data *byte, size uintptr, mode uint32, oneShot bool) int32
	addNetTap            func(ctx uint32, name string, mac *byte, features, flags uint32) int32
	addVsockPort2        func(ctx uint32, port uint32, path string, listen bool) int32
	disableImplicitVsock func(ctx uint32) int32
	addVsock             func(ctx uint32, tsiFeatures uint32) int32
	setKernelConsole     func(ctx uint32, id string) int32
	startEnter           func(ctx uint32) int32
}

// load opens the ABI 1 libkrun by absolute path, so a libkrun 2.x install is never loaded by mistake.
func load(dir string) (*api, error) {
	path := filepath.Join(dir, libkrunName)

	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}

	var a api

	symbols := []struct {
		name string
		fn   any
	}{
		{"krun_set_log_level", &a.setLogLevel},
		{"krun_create_ctx", &a.createCtx},
		{"krun_set_vm_config", &a.setVMConfig},
		{"krun_set_root", &a.setRoot},
		{"krun_set_env", &a.setEnv},
		{"krun_add_net_unixgram", &a.addNetUnixgram},
		{"krun_add_virtiofs4", &a.addVirtiofs4},
		{"krun_fs_add_overlay_file", &a.addOverlayFile},
		{"krun_add_net_tap", &a.addNetTap},
		{"krun_add_vsock_port2", &a.addVsockPort2},
		{"krun_disable_implicit_vsock", &a.disableImplicitVsock},
		{"krun_add_vsock", &a.addVsock},
		{"krun_set_kernel_console", &a.setKernelConsole},
		{"krun_start_enter", &a.startEnter},
	}

	var missing []string

	for _, s := range symbols {
		if err := register(handle, s.name, s.fn); err != nil {
			missing = append(missing, s.name)
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("%s does not export %s", path, strings.Join(missing, ", "))
	}

	return &a, nil
}

// Enter starts the VM described by cfg with libkrun from dir.
// On success it does not return: libkrun exits the process with the workload's exit code.
// It never sets the exec command, working directory, or resource limits, because the guest reads them from /.krun_config.json.
func Enter(dir string, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	krun, err := load(dir)
	if err != nil {
		return err
	}

	if err := check("krun_set_log_level", krun.setLogLevel(cfg.LogLevel)); err != nil {
		return err
	}

	rc := krun.createCtx()
	if rc < 0 {
		return check("krun_create_ctx", rc)
	}

	ctx := uint32(rc)

	if err := check("krun_set_vm_config", krun.setVMConfig(ctx, cfg.CPUs, cfg.MemoryMiB)); err != nil {
		return err
	}

	if err := check("krun_set_root", krun.setRoot(ctx, cfg.Root)); err != nil {
		return err
	}

	// A NULL list would copy the host environment into the guest, so pass an empty list.
	env := []*byte{nil}
	if err := check("krun_set_env", krun.setEnv(ctx, unsafe.Pointer(&env[0]))); err != nil {
		return err
	}

	if cfg.Net != nil {
		fd := int32(cfg.Net.FD) //nolint:gosec // validate checks the range.

		rc := krun.addNetUnixgram(ctx, nil, fd, &cfg.Net.MAC[0], compatNetFeatures, netFlagDHCPClient)
		if err := check("krun_add_net_unixgram", rc); err != nil {
			return err
		}
	}

	if err := addVolumes(krun, ctx, cfg); err != nil {
		return err
	}

	if err := addDevices(krun, ctx, cfg); err != nil {
		return err
	}

	// The implicit vsock device would forward guest sockets to the host (TSI).
	// Disabling it leaves a guest without a network device with no network at all.
	if err := check("krun_disable_implicit_vsock", krun.disableImplicitVsock(ctx)); err != nil {
		return err
	}

	// On macOS, libkrun sends the host time to the guest init over vsock. A device without TSI features and port map carries only that.
	// Linux has no such time sync, so the device would only add threads there. The symbol stays required on both, to keep one list.
	if runtime.GOOS == "darwin" || len(cfg.Vsock) > 0 {
		if err := check("krun_add_vsock", krun.addVsock(ctx, 0)); err != nil {
			return err
		}
	}

	if !anyTerminal() {
		if err := useQuietConsole(krun, ctx); err != nil {
			return err
		}
	}

	for _, p := range cfg.Vsock {
		if err := check("krun_add_vsock_port2", krun.addVsockPort2(ctx, p.Port, p.Path, p.Listen)); err != nil {
			return err
		}
	}

	return check("krun_start_enter", krun.startEnter(ctx))
}

// addDevices adds the TAP device and the extra virtio-fs shares.
func addDevices(krun *api, ctx uint32, cfg Config) error {
	if cfg.TAP != nil {
		// No flags: the guest configures its own address, without the DHCP client.
		if err := check("krun_add_net_tap", krun.addNetTap(ctx, cfg.TAP.Name, &cfg.TAP.MAC[0], compatNetFeatures, 0)); err != nil {
			return err
		}
	}

	for _, s := range cfg.Shares {
		if err := check("krun_add_virtiofs4", krun.addVirtiofs4(ctx, s.Tag, s.Path, 0, false, volumeSemantics)); err != nil {
			return err
		}
	}

	return nil
}

func (cfg Config) validate() error {
	switch {
	case !filepath.IsAbs(cfg.Root):
		return fmt.Errorf("VM root %q is not absolute", cfg.Root)
	case cfg.CPUs == 0:
		return errors.New("VM needs at least 1 CPU")
	case cfg.MemoryMiB == 0:
		return errors.New("VM needs at least 1 MiB of memory")
	case cfg.Net != nil && (cfg.Net.FD < 0 || cfg.Net.FD > math.MaxInt32):
		return fmt.Errorf("VM network fd %d is not valid", cfg.Net.FD)
	case slices.ContainsFunc(cfg.Volumes, func(v Volume) bool { return v.Tag == "" || !filepath.IsAbs(v.Host) }):
		return errors.New("VM volume needs a tag and an absolute host path")
	case len(cfg.Volumes) > 0 && len(cfg.MountHelper) == 0:
		return errors.New("VM volumes need the mount helper")
	case len(cfg.Volumes) > 0 && !filepath.IsAbs(cfg.MountHelperPath):
		return fmt.Errorf("VM mount helper path %q is not absolute", cfg.MountHelperPath)
	case cfg.Net != nil && cfg.TAP != nil:
		return errors.New("VM has both a unixgram and a TAP network device")
	case cfg.LogLevel > maxLogLevel:
		return fmt.Errorf("libkrun log level %d is above %d", cfg.LogLevel, maxLogLevel)
	}

	return nil
}

func check(name string, rc int32) error {
	if rc >= 0 {
		return nil
	}

	return fmt.Errorf("%s: %w", name, syscall.Errno(-rc))
}

// useQuietConsole makes the virtual terminal tty0 the kernel console, which the guest kernel registers at boot.
//
// The default console hvc0 appears only after a virtio handshake with this process. Under host load,
// the kernel starts the guest init before hvc0 exists, and the libkrun 1.19.8 init then loses its stdout (EBADF).
// The init still redirects stdout and stderr to the krun-stdout and krun-stderr ports, which exist only when no stdio fd is a terminal.
// tty0 shows nothing, so output to the console is dropped.
func useQuietConsole(krun *api, ctx uint32) error {
	return check("krun_set_kernel_console", krun.setKernelConsole(ctx, "tty0"))
}

func anyTerminal() bool {
	return slices.ContainsFunc([]int{0, 1, 2}, term.IsTerminal)
}

// mountHelper keeps the helper bytes alive for the VM, because libkrun serves them without a copy.
var mountHelper []byte

// addVolumes adds one virtio-fs device per volume and serves the mount helper in the root share.
func addVolumes(krun *api, ctx uint32, cfg Config) error {
	if len(cfg.Volumes) == 0 {
		return nil
	}

	// A zero DAX window size keeps the libkrun default.
	for _, v := range cfg.Volumes {
		if err := check("krun_add_virtiofs4", krun.addVirtiofs4(ctx, v.Tag, v.Host, 0, v.ReadOnly, volumeSemantics)); err != nil {
			return err
		}
	}

	mountHelper = cfg.MountHelper
	name := strings.TrimPrefix(cfg.MountHelperPath, "/")

	// One-shot: the init looks the helper up once to run it, and the workload never sees it.
	return check("krun_fs_add_overlay_file", krun.addOverlayFile(ctx, rootTag, name, &mountHelper[0], uintptr(len(mountHelper)), 0o100755, true))
}
