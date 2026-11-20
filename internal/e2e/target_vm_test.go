//go:build vm

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// vmBase is the busybox image that the real VM tests build the scenario images on.
// mirror.gcr.io serves Docker Hub library images without the Docker Hub rate limit.
const vmBase = "mirror.gcr.io/library/busybox:1.37.0"

// vmFlags keep many parallel VMs small.
var vmFlags = []string{"--cpus", "1", "--memory", "256M"}

// TestVMTarget runs the scenarios against the release build of sbxr with a real VM.
func TestVMTarget(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)

	RunAll(t, func(t *testing.T) Target {
		t.Helper()

		return &Binary{
			Path:     bin,
			CacheDir: vmCacheDir(t),
			Images:   refs,
			Flags:    vmFlags,
			Env:      vmEnv(),
			Caps: []Capability{
				Stdin, TTY, SplitStderr, SignalExit, SpecValidation, ExactConfig,
				Net, NetOff, Publish, DNS, EgressGuard, Volumes,
			},
		}
	})
}

// Binary is a target that runs an sbxr binary.
type Binary struct {
	Path string
	// CacheDir is the private SBXR_CACHE_DIR of this target.
	CacheDir string
	// Images maps scenario image names to references.
	Images map[string]string
	// Flags go after `run` in every run.
	Flags []string
	// Env is added to the environment of the sbxr process.
	Env  []string
	Caps []Capability
}

// Capabilities returns the capabilities set for this target.
func (b *Binary) Capabilities() []Capability { return b.Caps }

// CheckClean checks the private cache directory of this target.
func (b *Binary) CheckClean() error { return CheckClean(b.CacheDir) }

// Run runs `sbxr run` with flags for w.
func (b *Binary) Run(ctx context.Context, w Workload) (Result, error) {
	cmd := exec.CommandContext(ctx, b.Path, b.args(w)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "SBXR_CACHE_DIR=" + b.CacheDir}, b.Env...)
	cmd.Stdin = strings.NewReader(w.Stdin)

	stdout := newWatchWriter(w.InterruptAfter)
	stderr := &publishWatcher{ctx: ctx, onPublished: w.Published, mu: sync.Mutex{}, buf: bytes.Buffer{}, seen: map[string]bool{}}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Start()
	if err != nil {
		return Result{}, fmt.Errorf("start sbxr: %w", err)
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
		return Result{}, fmt.Errorf("run sbxr: %w (stderr %q)", err, stderr.String())
	}

	code := cmd.ProcessState.ExitCode()

	return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, Class: classify(code, stderr.String())}, nil
}

func (b *Binary) args(w Workload) []string {
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

// watchWriter collects output and closes seen once the output contains want.
type watchWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	seen chan struct{}
	done bool
}

func newWatchWriter(want string) *watchWriter {
	return &watchWriter{mu: sync.Mutex{}, buf: bytes.Buffer{}, want: want, seen: make(chan struct{}), done: false}
}

func (w *watchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)

	if !w.done && w.want != "" && strings.Contains(w.buf.String(), w.want) {
		w.done = true
		close(w.seen)
	}

	return len(p), nil
}

func (w *watchWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}

// publishedLine matches the debug log line of a published port, for example "published 127.0.0.1:8080 -> 80/tcp".
var publishedLine = regexp.MustCompile(`published (\S+) -> (\d+)/tcp`)

// publishWatcher collects stderr and calls onPublished in its own goroutine for each published port that the log names.
type publishWatcher struct {
	ctx         context.Context //nolint:containedctx // The callbacks run while the workload runs.
	onPublished func(ctx context.Context, guestPort uint16, hostAddr string)

	mu   sync.Mutex
	buf  bytes.Buffer
	seen map[string]bool
}

func (p *publishWatcher) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.buf.Write(b)

	if p.onPublished == nil {
		return len(b), nil
	}

	for _, m := range publishedLine.FindAllStringSubmatch(p.buf.String(), -1) {
		port, err := strconv.ParseUint(m[2], 10, 16)
		if err != nil || p.seen[m[0]] {
			continue
		}

		p.seen[m[0]] = true

		go p.onPublished(p.ctx, uint16(port), m[1])
	}

	return len(b), nil
}

func (p *publishWatcher) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.buf.String()
}

// runArgs returns the arguments of `sbxr run` with the small VM flags and then args.
func runArgs(args ...string) []string {
	return append(append([]string{"run"}, vmFlags...), args...)
}

// vmProcess returns the ID of the VM process of the one run in cacheDir.
func vmProcess(t *testing.T, cacheDir string) int {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", "__vm "+cacheDir).Output()
	if err != nil {
		t.Fatalf("find the VM process: %v", err)
	}

	pid, err := strconv.Atoi(strings.Fields(string(out))[0])
	if err != nil {
		t.Fatal(err)
	}

	return pid
}

// waitExit waits at most 10 seconds for the started cmd and returns its exit code.
func waitExit(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait() // The exit code shows the outcome.

		close(done)
	}()

	select {
	case <-done:
		return cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()

		t.Fatal("sbxr did not exit")

		return 0
	}
}

// waitForNoProcess waits until no process mentions dir. A killed parent cannot remove its run state.
func waitForNoProcess(t *testing.T, dir string) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, err := exec.CommandContext(t.Context(), "ps", "-A", "-ww", "-o", "pid=,args=").Output()
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(out), dir) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("a process for %s remains", dir)
		}
	}
}

// vmCacheDir returns a cache directory for one test.
// On Linux, cache files belong to subordinate ids, so a cleanup prunes the cache before the temporary directory goes.
func vmCacheDir(t *testing.T) string {
	t.Helper()

	bin := vmBinary(t)
	dir := filepath.Join(t.TempDir(), "cache")

	t.Cleanup(func() {
		// Only image work, which runs inside the user namespace, creates the images directory.
		if _, err := os.Stat(filepath.Join(dir, "images")); err != nil {
			return
		}

		cmd := exec.CommandContext(context.Background(), bin, "prune", "--force")
		cmd.Env = vmEnv(dir)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("prune %s: %v: %s", dir, err, out)
		}
	})

	return dir
}

// vmEnv returns the environment for sbxr: PATH, SBXR_LIB_DIR when set, and the cache directory when given.
func vmEnv(cacheDir ...string) []string {
	environ := []string{"PATH=" + os.Getenv("PATH")}

	if lib := os.Getenv("SBXR_LIB_DIR"); lib != "" {
		environ = append(environ, "SBXR_LIB_DIR="+lib)
	}

	if len(cacheDir) > 0 {
		environ = append(environ, "SBXR_CACHE_DIR="+cacheDir[0])
	}

	return environ
}

var (
	vmImagesOnce sync.Once
	vmRefs       map[string]string
	errVMImages  error
)

// vmBinary returns the sbxr binary under test: SBXR_BIN when set, else the signed release build.
// On Ubuntu with the AppArmor restriction, pass SBXR_BIN=/usr/local/bin/sbxr with the profile installed.
func vmBinary(t *testing.T) string {
	t.Helper()

	RequireHypervisor(t)

	if bin := os.Getenv("SBXR_BIN"); bin != "" {
		return bin
	}

	return releaseBinary(t)
}

// vmImages serves the scenario images built on vmBase for the rest of the test process.
func vmImages(t *testing.T) map[string]string {
	t.Helper()

	vmImagesOnce.Do(func() {
		ref, err := name.ParseReference(vmBase)
		if err != nil {
			errVMImages = err

			return
		}

		base, err := remote.Image(ref, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
		if err != nil {
			errVMImages = err

			return
		}

		vmRefs, _, errVMImages = ServeImages(context.Background(), base)
	})

	if errVMImages != nil {
		t.Fatal(errVMImages)
	}

	return vmRefs
}
