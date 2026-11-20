//go:build vm

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestVMTarget runs the scenarios against the release build of sbxr with a real VM.
func TestVMTarget(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)

	runAll(t, func(t *testing.T) target {
		t.Helper()

		return &vmTarget{
			Path:     bin,
			CacheDir: vmCacheDir(t),
			Images:   refs,
			Flags:    vmFlags,
			Env:      vmEnv(),
			Caps: []capability{
				capStdin, capTTY, capSplitStderr, capSignalExit, capSpecValidation, capExactConfig,
				capNet, capNetOff, capPublish, capDNS, capEgressGuard, capVolumes,
			},
		}
	})
}

// vmTarget is a target that runs an sbxr binary.
type vmTarget struct {
	Path string
	// CacheDir is the private SBXR_CACHE_DIR of this target.
	CacheDir string
	// Images maps scenario image names to references.
	Images map[string]string
	// Flags go after `run` in every run.
	Flags []string
	// Env is added to the environment of the sbxr process.
	Env  []string
	Caps []capability
}

// Capabilities returns the capabilities set for this target.
func (b *vmTarget) Capabilities() []capability { return b.Caps }

// CheckClean checks the private cache directory of this target.
func (b *vmTarget) CheckClean() error { return checkClean(b.CacheDir) }

// Run runs `sbxr run` with flags for w.
func (b *vmTarget) Run(ctx context.Context, w workload) (runResult, error) {
	cmd := exec.CommandContext(ctx, b.Path, b.args(w)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "SBXR_CACHE_DIR=" + b.CacheDir}, b.Env...)
	cmd.Stdin = strings.NewReader(w.Stdin)
	cmd.WaitDelay = 5 * time.Second

	stdout := newWatchWriter(w.InterruptAfter)
	stderr := &publishWatcher{ctx: ctx, onPublished: w.Published, mu: sync.Mutex{}, buf: bytes.Buffer{}, seen: map[string]bool{}}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Start()
	if err != nil {
		return runResult{}, fmt.Errorf("start sbxr: %w", err)
	}

	if w.InterruptAfter != "" {
		sig := syscall.SIGINT
		if w.Terminate {
			sig = syscall.SIGTERM
		}

		go func() {
			select {
			case <-stdout.seen:
				_ = cmd.Process.Signal(sig) // The exit code shows whether the signal arrived.
			case <-ctx.Done():
			}
		}()
	}

	err = cmd.Wait()

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return runResult{}, fmt.Errorf("run sbxr: %w (stderr %q)", err, stderr.String())
	}

	code := cmd.ProcessState.ExitCode()

	return runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, Class: classify(code, stderr.String())}, nil
}

func (b *vmTarget) args(w workload) []string {
	args := append([]string{"run"}, b.Flags...)

	for _, kv := range w.Env {
		args = append(args, "-e", kv)
	}

	if w.Workdir != "" {
		args = append(args, "-w", w.Workdir)
	}

	if w.User != "" {
		args = append(args, "-u", w.User)
	}

	if w.Interactive {
		args = append(args, "-i")
	}

	if w.TTY {
		args = append(args, "-t")
	}

	for _, v := range w.Volumes {
		flag := v.Host + ":" + v.Guest
		if v.ReadOnly {
			flag += ":ro"
		}

		args = append(args, "-v", flag)
	}

	if w.Net {
		args = append(args, "--net")
	}

	if w.AllowLAN {
		args = append(args, "--allow-lan")
	}

	// The debug log names the bound address of each published port.
	if len(w.Publish) > 0 {
		args = append(args, "--debug")
	}

	for _, p := range w.Publish {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p.Host, p.Guest))
	}

	image := w.Image
	if ref, ok := b.Images[image]; ok {
		image = ref
	}

	return append(append(args, image), w.Args...)
}
