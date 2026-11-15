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
	// ID identifies the image for its store, for example its manifest digest.
	ID     string
	Config ImageConfig
}

// Images is the image store.
type Images interface {
	// Pull returns the image for ref, downloading it when policy allows.
	Pull(ctx context.Context, ref string, policy spec.Pull) (Image, error)
	// NewRoot creates a private root file system for one run inside dir and returns its path.
	NewRoot(ctx context.Context, img Image, dir string) (string, error)
}

// Runner runs workloads.
type Runner struct {
	Images Images
	Checks []preflight.Check
	// Exe is the binary that runs the VM child mode, usually the running sbxr binary.
	Exe string
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

	r.Log.Debug("run state", "state", "pulling", "image", s.Image)

	img, err := r.Images.Pull(ctx, s.Image, s.Pull)
	if err != nil {
		return ExitSetup, &spec.Error{Source: "image", Value: s.Image, Problem: err.Error()}
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

	defer r.cleanup(dir)

	err = r.prepare(ctx, s, img, dir)
	if err != nil {
		return ExitSetup, err
	}

	r.Log.Debug("run state", "state", "running")

	code, err := r.start(ctx, s, dir)

	r.Log.Debug("run state", "state", "exited", "code", code)

	return code, err
}

func (r *Runner) validate(ctx context.Context, s spec.Spec) error {
	report := preflight.Run(ctx, r.Checks)
	if failed := report.Failures(); len(failed) > 0 {
		return &spec.Error{Source: "preflight", Value: failed[0].ID, Problem: failed[0].Detail, Fix: failed[0].Fix}
	}

	if len(s.Volumes) > 0 {
		return &spec.Error{Source: "-v", Problem: "volumes are not supported yet"}
	}

	if s.Net {
		return &spec.Error{Source: "--net", Problem: "networking is not supported yet"}
	}

	return nil
}

func (r *Runner) prepare(ctx context.Context, s spec.Spec, img Image, dir string) error {
	rootPath, err := r.Images.NewRoot(ctx, img, dir)
	if err != nil {
		return &spec.Error{Source: "image", Value: s.Image, Problem: err.Error()}
	}

	cfg, warnings, err := Workload(s, img.Config)
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

	err = vm.WriteMachine(dir, vm.Machine{Root: rootPath, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB})
	if err != nil {
		return fmt.Errorf("prepare vm: %w", err)
	}

	return nil
}

func (r *Runner) cleanup(dir string) {
	err := os.RemoveAll(dir)
	if err != nil {
		r.Log.Error("remove run directory", "dir", dir, "err", err)

		return
	}

	r.Log.Debug("run state", "state", "cleaned")
}

// signalError is the context cause when a signal stops the run.
type signalError struct{ sig syscall.Signal }

func (e signalError) Error() string { return "stopped by " + e.sig.String() }

// RunsDir returns the runs directory inside the cache directory.
func RunsDir(cacheDir string) string { return filepath.Join(cacheDir, "runs") }
