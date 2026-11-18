package scenario_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/runspec"
	"github.com/albertsko/sbxr/internal/scenario"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

// reference is an in-process target that checks the scenarios themselves without the sbxr binary.
// It validates with runspec.Parse, prepares the root with the image store, and runs the fake guest init directly.
type reference struct {
	images   image.Store
	refs     map[string]string
	cacheDir string
}

func newReference(t *testing.T, refs map[string]string) *reference {
	t.Helper()

	cacheDir := filepath.Join(t.TempDir(), "cache")

	cache, err := image.Open(cacheDir, image.OwnershipXattr)
	if err != nil {
		t.Fatal(err)
	}

	return &reference{images: image.Store{Cache: cache, Overlay: false, Progress: nil}, refs: refs, cacheDir: cacheDir}
}

func (r *reference) Capabilities() []scenario.Capability {
	return []scenario.Capability{scenario.Stdin, scenario.SplitStderr}
}

func (r *reference) CheckClean() error {
	return scenario.CheckClean(r.cacheDir) //nolint:wrapcheck // The message is complete.
}

func (r *reference) Run(ctx context.Context, w scenario.Workload) (scenario.Result, error) {
	s, err := runspec.Parse(runspec.Input{Image: r.refs[w.Image], Args: w.Args, Env: w.Env, Workdir: w.Workdir, User: w.User}, 2)
	if err != nil {
		return scenario.Result{ExitCode: 125, Class: scenario.InvalidSpec}, nil
	}

	img, err := r.images.Pull(ctx, s.Image, s.Pull)
	if err != nil {
		return scenario.Result{ExitCode: 125, Class: scenario.Image}, nil
	}

	cfg, _, err := run.Workload(s, img.Config, func(user string) (uint32, uint32, error) {
		return r.images.ResolveUser(img, user)
	})
	if err != nil {
		return scenario.Result{ExitCode: 125, Class: scenario.Image}, nil
	}

	dir, err := os.MkdirTemp(run.RunsDir(r.cacheDir), "")
	if err != nil {
		return scenario.Result{}, fmt.Errorf("create run directory: %w", err)
	}
	defer os.RemoveAll(dir)

	root, err := r.newRoot(ctx, img, dir, cfg)
	if err != nil {
		return scenario.Result{}, err
	}
	defer root.Close()

	stdin := ""
	if w.Interactive {
		stdin = w.Stdin
	}

	var stdout, stderr bytes.Buffer

	code := fake.Boot(root, fake.Stdio{In: strings.NewReader(stdin), Out: &stdout, Err: &stderr})

	class := scenario.OK

	switch code {
	case 126:
		class = scenario.NotExecutable
	case 127:
		class = scenario.NotFound
	}

	return scenario.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, Class: class}, nil
}

func (r *reference) newRoot(ctx context.Context, img run.Image, dir string, cfg krunconfig.Config) (*os.Root, error) {
	path, _, err := r.images.NewRoot(ctx, img, dir)
	if err != nil {
		return nil, fmt.Errorf("create root: %w", err)
	}

	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open root: %w", err)
	}

	err = krunconfig.Write(root, cfg)
	if err != nil {
		root.Close()

		return nil, fmt.Errorf("write config: %w", err)
	}

	return root, nil
}

func TestReferenceTarget(t *testing.T) {
	t.Parallel()

	refs := scenario.FakeImages(t)

	scenario.RunAll(t, func(t *testing.T) scenario.Target {
		t.Helper()

		return newReference(t, refs)
	})
}

// wrongExit breaks the exit code of every run.
type wrongExit struct{ *reference }

func (w wrongExit) Run(ctx context.Context, wl scenario.Workload) (scenario.Result, error) {
	res, err := w.reference.Run(ctx, wl)
	res.ExitCode++

	return res, err
}

// leftover leaves a process that names its cache directory behind after every run.
type leftover struct {
	*reference

	t *testing.T
}

func (l leftover) Run(ctx context.Context, wl scenario.Workload) (scenario.Result, error) {
	// The trailing exit keeps sh alive, so that its command line names the cache directory.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "sh", "-c", "sleep 60; exit", l.cacheDir)

	err := cmd.Start()
	if err != nil {
		return scenario.Result{}, fmt.Errorf("start leftover process: %w", err)
	}

	l.t.Cleanup(func() {
		_ = cmd.Process.Kill() // The process may be gone already.
		_ = cmd.Wait()
	})

	return l.reference.Run(ctx, wl)
}

// TestScenariosCatchBrokenTargets checks that every scenario fails on a wrong exit code
// and that the cleanup check fails on a leftover process.
func TestScenariosCatchBrokenTargets(t *testing.T) {
	t.Parallel()

	refs := scenario.FakeImages(t)
	targets := map[string]func(t *testing.T) scenario.Target{
		"wrong exit code": func(t *testing.T) scenario.Target {
			t.Helper()

			return wrongExit{newReference(t, refs)}
		},
		"leftover process": func(t *testing.T) scenario.Target {
			t.Helper()

			return leftover{newReference(t, refs), t}
		},
	}

	for name, newTarget := range targets {
		for _, s := range scenario.All() {
			t.Run(name+"/"+s.Name, func(t *testing.T) {
				t.Parallel()

				target := newTarget(t)
				if s.Pending != "" || len(scenario.Lacking(target, s)) > 0 {
					t.Skip("the scenario does not run on this target")
				}

				err := scenario.Verify(t.Context(), s, target)
				if err == nil {
					t.Fatal("Verify() = nil, want an error")
				}

				wantPrefix := map[string]string{"wrong exit code": "scenario:", "leftover process": "cleanup:"}[name]
				if !strings.Contains(err.Error(), wantPrefix) {
					t.Errorf("Verify() = %v, want a %s error", err, wantPrefix)
				}
			})
		}
	}
}
