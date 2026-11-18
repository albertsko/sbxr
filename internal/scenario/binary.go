package scenario

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Packages of the sbxr binaries that tests build.
const (
	ReleasePkg = "github.com/albertsko/sbxr/cmd/sbxr"
	FakePkg    = "github.com/albertsko/sbxr/internal/testbin/sbxr-fake"
)

// Build builds the sbxr binary in pkg into out with cgo off, like the release.
// The release build first generates the guest mount helper, which it embeds.
func Build(ctx context.Context, out, pkg string) error {
	if pkg == ReleasePkg {
		gen := exec.CommandContext(ctx, "go", "generate", "github.com/albertsko/sbxr/internal/guestmount")
		if output, err := gen.CombinedOutput(); err != nil {
			return fmt.Errorf("generate the mount helper: %w\n%s", err, output)
		}
	}

	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, pkg)

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
	// Images maps scenario image names to references.
	Images map[string]string
	// Flags go after `run` in every run.
	Flags []string
	// Env is added to the environment of the sbxr process.
	Env  []string
	Caps []Capability
}

// Fake returns a target that runs the fake build of sbxr at bin with the scenario images in refs.
func Fake(t *testing.T, bin string, refs map[string]string) *Binary {
	t.Helper()

	return &Binary{
		Path:     bin,
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Images:   refs,
		Flags:    nil,
		Env:      nil,
		Caps:     []Capability{Stdin, TTY, SplitStderr, SignalExit, SpecValidation, ExactConfig, Net, NetOff, Publish, DNS, EgressGuard, Volumes},
	}
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
