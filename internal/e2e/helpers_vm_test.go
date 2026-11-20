//go:build vm

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
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

// vmTimeout bounds each sbxr process of the vm tests, so that one hang fails its test and not the whole suite.
const vmTimeout = time.Minute

// readyTimeout bounds each wait for a sign that a run is ready.
const readyTimeout = 30 * time.Second

// psTimeout bounds each process listing.
const psTimeout = 10 * time.Second

// vmCommand returns a command that runs bin with environ and args and that is killed after vmTimeout.
// The cleanup kills the process and reaps it, unless the test already waited for it.
func vmCommand(t *testing.T, bin string, environ []string, args ...string) *exec.Cmd {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), vmTimeout)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = environ
	// The VM process inherits the output pipes, so Wait must not wait for them without a limit.
	cmd.WaitDelay = 5 * time.Second

	t.Cleanup(func() {
		cancel()

		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Wait() // The test failed before it waited, so only the reaping matters.
		}
	})

	return cmd
}

// readReady runs read, which reads a sign of readiness from the pipe r of the started cmd, with a deadline of readyTimeout.
// When read fails, readReady kills cmd and fails t, and the owner of cmd reaps it.
func readReady(t *testing.T, cmd *exec.Cmd, r io.Reader, read func() error) {
	t.Helper()

	pipe, ok := r.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		t.Fatalf("%T has no read deadline", r)
	}

	err := pipe.SetReadDeadline(time.Now().Add(readyTimeout))
	if err == nil {
		err = read()
	}

	if err == nil {
		err = pipe.SetReadDeadline(time.Time{})
	}

	if err != nil {
		_ = cmd.Process.Kill()

		t.Fatalf("wait for the run to get ready: %v", err)
	}
}

// readLines reads n lines from r for readReady.
func readLines(r *bufio.Reader, n int) func() error {
	return func() error {
		for range n {
			_, err := r.ReadString('\n')
			if err != nil {
				return err //nolint:wrapcheck // readReady names the step.
			}
		}

		return nil
	}
}

// readText reads len(text) bytes from r for readReady and checks that they are text.
func readText(r io.Reader, text string) func() error {
	return func() error {
		buf := make([]byte, len(text))

		_, err := io.ReadFull(r, buf)
		if err == nil && string(buf) != text {
			err = fmt.Errorf("read %q, want %q", buf, text)
		}

		return err
	}
}

// runArgs returns the arguments of `sbxr run` with the small VM flags and then args.
func runArgs(args ...string) []string {
	return append(append([]string{"run"}, vmFlags...), args...)
}

// vmProcess returns the ID of the VM process of the one run in cacheDir.
func vmProcess(t *testing.T, cacheDir string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pgrep", "-f", "__vm "+cacheDir).Output()
	if err != nil {
		t.Fatalf("find the VM process: %v", err)
	}

	pid, err := strconv.Atoi(strings.Fields(string(out))[0])
	if err != nil {
		t.Fatal(err)
	}

	return pid
}

// runnerOf returns the runner of the VM process vmPID in the run that the test started as cmd.
// On macOS, cmd is the runner. On Linux, cmd is the user namespace wrapper, and the runner is its child.
// The runner gets SIGTERM and cleans up when the wrapper dies, so a test that must kill the runner cannot kill cmd instead.
func runnerOf(t *testing.T, cmd *exec.Cmd, vmPID int) int {
	t.Helper()

	runner := parentOf(t, vmPID)
	if runner == cmd.Process.Pid || parentOf(t, runner) == cmd.Process.Pid {
		return runner
	}

	t.Fatalf("the parent %d of the VM process %d is neither sbxr %d nor its child", runner, vmPID, cmd.Process.Pid)

	return 0
}

// parentOf returns the parent process ID of pid.
func parentOf(t *testing.T, pid int) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("find the parent of %d: %v", pid, err)
	}

	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}

	return ppid
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

		<-done

		t.Fatal("sbxr did not exit")

		return 0
	}
}

// waitForNoProcess waits until no process mentions dir. A killed parent cannot remove its run state.
func waitForNoProcess(t *testing.T, dir string) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
		out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,args=").Output()

		cancel()

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

// waitForGone waits until the process pid has exited and its parent has reaped it.
func waitForGone(t *testing.T, pid int) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d remains", pid)
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

		ctx, cancel := context.WithTimeout(context.Background(), vmTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, bin, "prune", "--force")
		cmd.Env = vmEnv(dir)
		cmd.WaitDelay = 5 * time.Second

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

	requireHypervisor(t)

	if bin := os.Getenv("SBXR_BIN"); bin != "" {
		return bin
	}

	return releaseBinary(t)
}

// vmImages serves the scenario images built on vmBase for the rest of the test process.
func vmImages(t *testing.T) map[string]string {
	t.Helper()

	vmImagesOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		ref, err := name.ParseReference(vmBase)
		if err != nil {
			errVMImages = err

			return
		}

		base, err := remote.Image(ref, remote.WithContext(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
		if err != nil {
			errVMImages = err

			return
		}

		vmRefs, _, errVMImages = serveImages(ctx, base)
	})

	if errVMImages != nil {
		t.Fatal(errVMImages)
	}

	return vmRefs
}
