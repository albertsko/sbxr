//go:build darwin || linux

package libkrun

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
	// LogLevel is the libkrun log level, from 0 (off) to 5 (trace).
	LogLevel uint32
}

// Net is a virtio-net device backed by a connected unixgram socket that the VM process inherited.
type Net struct {
	FD  int
	MAC [6]byte
}

// api holds the libkrun functions that sbxr uses. Each one returns 0 or a negative errno.
type api struct {
	setLogLevel          func(level uint32) int32
	createCtx            func() int32
	setVMConfig          func(ctx uint32, cpus uint8, ramMiB uint32) int32
	setRoot              func(ctx uint32, path string) int32
	setEnv               func(ctx uint32, envp unsafe.Pointer) int32
	addNetUnixgram       func(ctx uint32, path unsafe.Pointer, fd int32, mac *byte, features, flags uint32) int32
	disableImplicitVsock func(ctx uint32) int32
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
		{"krun_disable_implicit_vsock", &a.disableImplicitVsock},
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

	// Without a vsock device the guest has no socket forwarding (TSI), so without a network device it has no network at all.
	if err := check("krun_disable_implicit_vsock", krun.disableImplicitVsock(ctx)); err != nil {
		return err
	}

	if !anyTerminal() {
		if err := useQuietConsole(krun, ctx); err != nil {
			return err
		}
	}

	return check("krun_start_enter", krun.startEnter(ctx))
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
