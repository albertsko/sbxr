// Package run runs one workload in a VM: validating, pulling, preparing, running, exited, cleaned.
//
// The runner is the parent side of a run. It prepares the run directory, starts the VM process,
// wires stdio and the terminal, forwards signals, and removes all run state on every path.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/runspec"
	"github.com/albertsko/sbxr/internal/vm"
)

// Network is the network of one run.
type Network interface {
	// VMFile is the socket that the VM process attaches its network device to.
	VMFile() *os.File
	// GuestMAC is the MAC address that the VM must use.
	GuestMAC() net.HardwareAddr
	// Close stops the network and removes its listeners.
	Close() error
}

// NetworkConfig describes the network of one run.
type NetworkConfig struct {
	AllowLAN bool
	Publish  []runspec.Port
	Log      *slog.Logger
}

// Runner runs workloads.
type Runner struct {
	Images image.Store
	Checks []preflight.Check
	// Network starts the network of a run with --net.
	Network func(ctx context.Context, cfg NetworkConfig) (Network, error)
	// MountHelper reports whether this build has the guest mount helper for volumes (see internal/guestmount).
	MountHelper func() error
	// Isolate, when set, runs the rest of the run in another process and returns its exit code.
	// On Linux, it runs the same command again inside a user namespace.
	Isolate func(stdin, stdout, stderr *os.File) (int, error)
	// Exe is the binary that runs the VM child mode, usually the running sbxr binary.
	Exe string
	// VMEnv is added to the environment of the VM process.
	VMEnv []string
	// VMLogLevel is the hypervisor log level, from 0 (off) to 5 (trace).
	VMLogLevel uint32
	// RunsDir holds one directory per active run.
	RunsDir string
	Log     *slog.Logger
	Stdin   *os.File
	Stdout  *os.File
	Stderr  *os.File
}

// Run runs the workload and returns its exit code.
// An error means that sbxr failed, and the exit code is then vm.ExitSetup.
// A SIGINT, SIGTERM, or SIGHUP stops the run at once with exit code 130, 143, or 129 and no error.
func (r *Runner) Run(ctx context.Context, s runspec.Spec) (int, error) {
	ctx, stop := withSignals(ctx)
	defer stop()

	code, err := r.run(ctx, s)

	if sig, ok := errors.AsType[signalError](context.Cause(ctx)); ok {
		r.Log.Debug("run stopped", "signal", sig.sig)

		return 128 + int(sig.sig), nil
	}

	return code, err
}

func (r *Runner) run(ctx context.Context, s runspec.Spec) (int, error) {
	r.Log.Debug("run state", "state", "validating")

	err := r.validate(ctx, s)
	if err != nil {
		return vm.ExitSetup, err
	}

	if r.Isolate != nil {
		return r.Isolate(r.Stdin, r.Stdout, r.Stderr)
	}

	hold, err := r.Images.Hold(ctx)
	if err != nil {
		return vm.ExitSetup, fmt.Errorf("hold image cache: %w", err)
	}
	defer hold.Close() //nolint:errcheck // Closing a lock file releases the lock even on error.

	r.Log.Debug("run state", "state", "pulling", "image", s.Image)

	img, err := r.Images.Pull(ctx, s.Image, s.Pull)
	if err != nil {
		return vm.ExitSetup, imageError(s, err)
	}

	r.Log.Debug("run state", "state", "preparing")

	cfg, err := r.workload(s, img)
	if err != nil {
		return vm.ExitSetup, err
	}

	// The network starts before any run state, so that a busy publish port fails early.
	var network Network

	if s.Net {
		network, err = r.Network(ctx, NetworkConfig{AllowLAN: s.AllowLAN, Publish: s.Publish, Log: r.Log})
		if err != nil {
			return vm.ExitSetup, fmt.Errorf("network: %w", err)
		}

		defer func() {
			if err := network.Close(); err != nil {
				r.Log.Warn("stop network", "err", err)
			}
		}()
	}

	dir, lock, unlock, err := newRunDir(r.RunsDir, r.Log)
	if err != nil {
		return vm.ExitSetup, err
	}
	defer unlock()

	release := func() error { return nil }
	defer func() { r.cleanup(dir, release) }()

	release, err = r.prepare(s, img, cfg, dir, network)
	if err != nil {
		return vm.ExitSetup, err
	}

	r.Log.Debug("run state", "state", "running")

	extra := []*os.File{hold, lock}
	if network != nil {
		extra = append(extra, network.VMFile())
	}

	code, err := r.start(ctx, s, dir, extra)

	r.Log.Debug("run state", "state", "exited", "code", code)

	return code, err
}

func (r *Runner) validate(ctx context.Context, s runspec.Spec) error {
	if s.Interactive && s.TTY && !term.IsTerminal(int(r.Stdin.Fd())) {
		return &runspec.Error{Source: "-t", Problem: "the input device is not a terminal", Fix: "drop -t, or run sbxr from a terminal"}
	}

	err := preflight.Run(ctx, r.Checks).Err()
	if err != nil {
		return err //nolint:wrapcheck // The preflight error is a complete user message.
	}

	return r.checkVolumes(s.Volumes)
}

// workload resolves and checks the guest init config before any run state exists.
func (r *Runner) workload(s runspec.Spec, img *image.Image) (krunconfig.Config, error) {
	resolveUser := func(user string) (uint32, uint32, error) {
		uid, gid, err := img.ResolveUser(user)
		if err != nil {
			return 0, 0, imageError(s, err)
		}

		return uid, gid, nil
	}

	cfg, warnings, err := Workload(s, img.Config, resolveUser)
	if err != nil {
		return krunconfig.Config{}, err
	}

	for _, w := range warnings {
		r.Log.Warn(w)
	}

	_, err = krunconfig.Marshal(cfg)
	if err != nil {
		return krunconfig.Config{}, &runspec.Error{Source: "workload", Problem: err.Error()}
	}

	return cfg, nil
}

// prepare creates the run root and writes the workload config and the machine into the run directory.
// It returns the release function of the root, also on error.
func (r *Runner) prepare(s runspec.Spec, img *image.Image, cfg krunconfig.Config, dir string, network Network) (func() error, error) {
	rootPath, release, err := r.Images.NewRoot(img, dir)
	if err != nil {
		return func() error { return nil }, imageError(s, err)
	}

	return release, r.configure(s, cfg, dir, rootPath, network)
}

func (r *Runner) configure(s runspec.Spec, cfg krunconfig.Config, dir, rootPath string, network Network) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open run root: %w", err)
	}
	defer root.Close() //nolint:errcheck // The root is only read and written through Write.

	cfg, volumes := withVolumes(s.Volumes, cfg)

	err = krunconfig.Write(root, cfg)
	if err != nil {
		return fmt.Errorf("write workload config: %w", err)
	}

	m := vm.Machine{Root: rootPath, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, LogLevel: r.VMLogLevel, Net: nil, Volumes: volumes}
	if network != nil {
		m.Net = &vm.Net{FD: vm.NetFD, MAC: network.GuestMAC().String()}
	}

	err = vm.WriteMachine(dir, m)
	if err != nil {
		return fmt.Errorf("prepare vm: %w", err)
	}

	return nil
}

func (r *Runner) cleanup(dir string, release func() error) {
	err := release()
	if err != nil {
		r.Log.Error("release run root", "dir", dir, "err", err)
	}

	err = os.RemoveAll(dir)
	if err != nil {
		r.Log.Error("remove run directory", "dir", dir, "err", err)

		return
	}

	r.Log.Debug("run state", "state", "cleaned")
}

// imageError names the image in an image store error.
func imageError(s runspec.Spec, err error) error {
	return &runspec.Error{Source: "image", Value: s.Image, Problem: err.Error()}
}

// signalError is the context cause when a signal stops the run.
type signalError struct{ sig syscall.Signal }

func (e signalError) Error() string { return "stopped by " + e.sig.String() }

// RunsDir returns the runs directory inside the cache directory.
func RunsDir(cacheDir string) string { return filepath.Join(cacheDir, "runs") }
