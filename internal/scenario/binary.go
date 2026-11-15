package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

// Build builds the sbxr binary into out with cgo off, like the release.
func Build(ctx context.Context, out string, tags ...string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", strings.Join(tags, ","), "-o", out, "github.com/albertsko/sbxr/cmd/sbxr") //nolint:gosec // Tests choose the arguments.

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build sbxr: %w\n%s", err, output)
	}

	return nil
}

// Binary is a target that runs an sbxr binary.
type Binary struct {
	Path string
	// CacheDir is the private SBXR_CACHE_DIR of this target.
	CacheDir string
	// Env is added to the environment of the sbxr process.
	Env  []string
	Caps []Capability
}

// Fake returns a target that runs the fake build of sbxr at bin with the scenario images.
func Fake(t *testing.T, bin string) *Binary {
	t.Helper()

	dir := t.TempDir()
	images := filepath.Join(dir, "images")

	err := WriteFakeImages(images)
	if err != nil {
		t.Fatal(err)
	}

	return &Binary{
		Path:     bin,
		CacheDir: filepath.Join(dir, "cache"),
		Env:      []string{"SBXR_FAKE_IMAGES=" + images},
		Caps:     []Capability{Stdin, TTY, SplitStderr, SignalExit},
	}
}

// WriteFakeImages writes the scenario images for the fake build into dir.
func WriteFakeImages(dir string) error {
	images := map[string]run.ImageConfig{
		Busybox: {Cmd: []string{"sh"}, Env: []string{"PATH=/bin"}},
		Configured: {
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{"echo $A $B; id -u; id -g; pwd"},
			Env:        []string{"PATH=/bin", "A=image", "B=image"},
			WorkingDir: "/tmp",
			User:       "1000:1000",
		},
	}

	for ref, cfg := range images {
		err := fake.WriteImage(dir, ref, cfg)
		if err != nil {
			return fmt.Errorf("write image %s: %w", ref, err)
		}
	}

	return nil
}

// Capabilities returns the capabilities set for this target.
func (b *Binary) Capabilities() []Capability { return b.Caps }

// CheckClean checks the private cache directory of this target.
func (b *Binary) CheckClean() error { return CheckClean(b.CacheDir) }

// Run runs `sbxr run` with flags for w.
func (b *Binary) Run(ctx context.Context, w Workload) (Result, error) {
	cmd := exec.CommandContext(ctx, b.Path, b.args(w)...) //nolint:gosec // Path is the sbxr binary under test.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "SBXR_CACHE_DIR=" + b.CacheDir}, b.Env...)
	cmd.Stdin = strings.NewReader(w.Stdin)

	var stderr bytes.Buffer

	stdout := newWatchWriter(w.InterruptAfter)
	cmd.Stdout, cmd.Stderr = stdout, &stderr

	err := cmd.Start()
	if err != nil {
		return Result{}, fmt.Errorf("start sbxr: %w", err)
	}

	if w.InterruptAfter != "" {
		go func() {
			select {
			case <-stdout.seen:
				_ = cmd.Process.Signal(syscall.SIGINT) // The exit code shows whether the signal arrived.
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

	return Result{Stdout: stdout.String(), ExitCode: code, Class: classify(code, stderr.String())}, nil
}

func (b *Binary) args(w Workload) []string {
	args := []string{"run"}

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

	return append(append(args, w.Image), w.Args...)
}

// classify maps the CLI exit code and stderr to an error class.
// sbxr failures exit 125 and print a line that starts with "sbxr: <source>".
func classify(code int, stderr string) ErrorClass {
	sbxrErr, failed := "", false

	for line := range strings.Lines(stderr) {
		if after, ok := strings.CutPrefix(line, "sbxr: "); ok && code == 125 {
			sbxrErr, failed = after, true
		}
	}

	switch {
	case failed && strings.HasPrefix(sbxrErr, "preflight "):
		return Preflight
	case failed && strings.HasPrefix(sbxrErr, "image "):
		return Image
	case failed:
		return InvalidSpec
	case code == 126:
		return NotExecutable
	case code == 127:
		return NotFound
	case code == 130 || code == 143:
		return Interrupted
	default:
		return OK
	}
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
