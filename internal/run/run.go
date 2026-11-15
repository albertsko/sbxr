// Package run runs one workload in a VM: validating, pulling, preparing, running, exited, cleaned.
//
// The runner is the parent side of a run. It prepares the run directory, starts the VM process,
// wires stdio and the terminal, forwards signals, and removes all run state on every path.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// ExitSetup is the exit code when sbxr fails before or around the workload.
const ExitSetup = vm.ExitSetup

// ImageConfig is the part of an OCI image config that shapes the workload.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
}

// Image is a pulled image.
type Image struct {
	// ID identifies the image for its store, for example its cached root directory.
	ID     string
	Config ImageConfig
}

// Images is the image store.
type Images interface {
	// Hold keeps the store from removing images until the closer is closed.
	Hold(ctx context.Context) (io.Closer, error)
	// Pull returns the image for ref, downloading it when policy allows.
	Pull(ctx context.Context, ref string, policy spec.Pull) (Image, error)
	// ResolveUser resolves an image User value inside the image.
	ResolveUser(img Image, user string) (uid, gid uint32, err error)
	// NewRoot creates a private root file system for one run inside dir and returns its path.
	// release undoes what a removal of dir cannot, such as a mount.
	NewRoot(ctx context.Context, img Image, dir string) (root string, release func() error, err error)
}

// Runner runs workloads.
type Runner struct {
	Images Images
	Checks []preflight.Check
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
// An error means that sbxr failed, and the exit code is then ExitSetup.
// A SIGINT or SIGTERM stops the run at once with exit code 130 or 143 and no error.
func (r *Runner) Run(ctx context.Context, s spec.Spec) (int, error) {
	ctx, stop := withSignals(ctx)
	defer stop()

	code, err := r.run(ctx, s)

	if sig, ok := errors.AsType[signalError](context.Cause(ctx)); ok {
		r.Log.Debug("run stopped", "signal", sig.sig)

		return 128 + int(sig.sig), nil
	}

	return code, err
}

func (r *Runner) run(ctx context.Context, s spec.Spec) (int, error) {
	r.Log.Debug("run state", "state", "validating")

	err := r.validate(ctx, s)
	if err != nil {
		return ExitSetup, err
	}

	if r.Isolate != nil {
		return r.Isolate(r.Stdin, r.Stdout, r.Stderr)
	}

	hold, err := r.Images.Hold(ctx)
	if err != nil {
		return ExitSetup, fmt.Errorf("hold image cache: %w", err)
	}
	defer hold.Close() //nolint:errcheck // Closing a lock file releases the lock even on error.

	r.Log.Debug("run state", "state", "pulling", "image", s.Image)

	img, err := r.Images.Pull(ctx, s.Image, s.Pull)
	if err != nil {
		return ExitSetup, imageError(s, err)
	}

	r.Log.Debug("run state", "state", "preparing")

	err = os.MkdirAll(r.RunsDir, 0o700)
	if err != nil {
		return ExitSetup, fmt.Errorf("create runs directory: %w", err)
	}

	dir, err := os.MkdirTemp(r.RunsDir, "")
	if err != nil {
		return ExitSetup, fmt.Errorf("create run directory: %w", err)
	}

	release := func() error { return nil }
	defer func() { r.cleanup(dir, release) }()

	release, err = r.prepare(ctx, s, img, dir)
	if err != nil {
		return ExitSetup, err
	}

	r.Log.Debug("run state", "state", "running")

	code, err := r.start(ctx, s, dir)

	r.Log.Debug("run state", "state", "exited", "code", code)

	return code, err
}

func (r *Runner) validate(ctx context.Context, s spec.Spec) error {
	err := preflight.Run(ctx, r.Checks).Err()
	if err != nil {
		return err //nolint:wrapcheck // The preflight error is a complete user message.
	}

	if len(s.Volumes) > 0 {
		return &spec.Error{Source: "-v", Problem: "volumes are not supported yet"}
	}

	if s.Net {
		return &spec.Error{Source: "--net", Problem: "networking is not supported yet"}
	}

	return nil
}

// prepare creates the run root and writes the workload config and the machine into the run directory.
// It returns the release function of the root, also on error.
func (r *Runner) prepare(ctx context.Context, s spec.Spec, img Image, dir string) (func() error, error) {
	rootPath, release, err := r.Images.NewRoot(ctx, img, dir)
	if err != nil {
		return func() error { return nil }, imageError(s, err)
	}

	return release, r.configure(s, img, dir, rootPath)
}

func (r *Runner) configure(s spec.Spec, img Image, dir, rootPath string) error {
	resolveUser := func(user string) (uint32, uint32, error) {
		uid, gid, err := r.Images.ResolveUser(img, user)
		if err != nil {
			return 0, 0, imageError(s, err)
		}

		return uid, gid, nil
	}

	cfg, warnings, err := Workload(s, img.Config, resolveUser)
	if err != nil {
		return err
	}

	for _, w := range warnings {
		r.Log.Warn(w)
	}

	_, err = krunconfig.Marshal(cfg)
	if err != nil {
		return &spec.Error{Source: "workload", Problem: err.Error()}
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open run root: %w", err)
	}
	defer root.Close() //nolint:errcheck // The root is only read and written through Write.

	err = krunconfig.Write(root, cfg)
	if err != nil {
		return fmt.Errorf("write workload config: %w", err)
	}

	err = vm.WriteMachine(dir, vm.Machine{Root: rootPath, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, LogLevel: r.VMLogLevel})
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
func imageError(s spec.Spec, err error) error {
	return &spec.Error{Source: "image", Value: s.Image, Problem: err.Error()}
}

// signalError is the context cause when a signal stops the run.
type signalError struct{ sig syscall.Signal }

func (e signalError) Error() string { return "stopped by " + e.sig.String() }

// RunsDir returns the runs directory inside the cache directory.
func RunsDir(cacheDir string) string { return filepath.Join(cacheDir, "runs") }
