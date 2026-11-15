//go:build hv

package scenario_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/scenario"
)

// hvBase is the busybox image that the real VM tests build the scenario images on.
// mirror.gcr.io serves Docker Hub library images without the Docker Hub rate limit.
const hvBase = "mirror.gcr.io/library/busybox:1.37.0"

// hvFlags keep many parallel VMs small.
var hvFlags = []string{"--cpus", "1", "--memory", "256M"}

// TestHVCLITarget runs the scenarios against the release build of sbxr with a real VM.
func TestHVCLITarget(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	refs := hvImages(t)

	scenario.RunAll(t, func(t *testing.T) scenario.Target {
		t.Helper()

		return &scenario.Binary{
			Path:     bin,
			CacheDir: hvCacheDir(t),
			Images:   refs,
			Flags:    hvFlags,
			Env:      hvEnv(),
			Caps: []scenario.Capability{
				scenario.Stdin, scenario.TTY, scenario.SplitStderr, scenario.SignalExit, scenario.SpecValidation, scenario.ExactConfig,
				scenario.Net, scenario.NetOff, scenario.Publish, scenario.DNS, scenario.EgressGuard, scenario.Volumes,
			},
		}
	})
}

func TestHVDoctorPasses(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	out, code := sbxr(t, bin, hvEnv(hvCacheDir(t)), "doctor", "--json")

	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}

	err := json.Unmarshal([]byte(out), &report)
	if err != nil || code != 0 || !report.OK {
		t.Fatalf("sbxr doctor --json = %d, %q, %v; want 0 and ok", code, out, err)
	}
}

func TestHVFailedPreflightStartsNoVM(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	refs := hvImages(t)
	cacheDir := hvCacheDir(t)
	environ := append(hvEnv(cacheDir), "SBXR_LIB_DIR="+filepath.Join(t.TempDir(), "nolib"))

	out, code := sbxr(t, bin, environ, "run", refs[scenario.Busybox], "echo", "hi")
	if code != 125 || !strings.HasPrefix(out, `sbxr: preflight "libkrun": `) {
		t.Errorf("sbxr run = %d, %q; want 125 and a libkrun preflight error", code, out)
	}

	err := scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}

	out, code = sbxr(t, bin, environ, "doctor")

	_, fixAll, _ := strings.Cut(out, "Fix all:")
	if code != 1 || !strings.Contains(out, "fail  libkrun") || !strings.Contains(fixAll, "SBXR_LIB_DIR") {
		t.Errorf("sbxr doctor = %d, %q; want 1, a libkrun failure, and a combined fix", code, out)
	}
}

// TestHVAppArmorNeedsInstallPath checks the userns fix for a binary outside the profile paths.
func TestHVAppArmorNeedsInstallPath(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Skip("AppArmor does not restrict user namespaces here")
	}

	bin := filepath.Join(t.TempDir(), "sbxr")

	err = scenario.Build(t.Context(), bin)
	if err != nil {
		t.Fatal(err)
	}

	out, code := sbxr(t, bin, hvEnv(hvCacheDir(t)), "doctor")

	// The setup block at the end installs to /usr/local/bin, so only the report above it counts.
	out, _ = strings.CutSuffix(out, preflight.LinuxSetup)
	if code != 1 || !strings.Contains(out, "fail  userns") || !strings.Contains(out, "install sbxr to /usr/local/bin") || strings.Contains(out, "apparmor_parser") {
		t.Errorf("sbxr doctor = %d, %q; want a userns failure that asks for /usr/local/bin and prints no profile command", code, out)
	}
}

// TestHVStderrOnTerminalWithPipedStdout checks that each guest stream reaches its own host stream
// when only stderr is a terminal.
func TestHVStderrOnTerminalWithPipedStdout(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	refs := hvImages(t)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	term := &syncBuffer{}

	go func() { _, _ = io.Copy(term, ptmx) }() // The copy ends when the terminal closes.

	args := append(append([]string{"run", "-q"}, hvFlags...), refs[scenario.Busybox], "sh", "-c", "echo OUT; echo ERR >&2")
	cmd := exec.CommandContext(t.Context(), bin, args...)

	var stdout bytes.Buffer

	cmd.Env = hvEnv(hvCacheDir(t))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, &stdout, tty

	err = cmd.Run()
	if err != nil {
		t.Fatalf("sbxr run: %v (terminal %q)", err, term.String())
	}

	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(term.String(), "ERR") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}

	if stdout.String() != "OUT\n" || !strings.Contains(term.String(), "ERR") || strings.Contains(term.String(), "OUT") {
		t.Errorf("stdout %q, terminal %q; want OUT on stdout and ERR on the terminal", stdout.String(), term.String())
	}
}

// TestHVInteractiveShell types into an interactive shell on a terminal and resizes the terminal.
// libkrun 1.19.8 forwards terminal resizes to the guest only on Linux.
func TestHVInteractiveShell(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	refs := hvImages(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	err = pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}

	args := append(append([]string{"run", "-it"}, hvFlags...), refs[scenario.Busybox], "sh")
	cmd := exec.CommandContext(ctx, bin, args...)

	cmd.Env = append(hvEnv(hvCacheDir(t)), "TERM=xterm")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	out := &syncBuffer{}

	go func() { _, _ = io.Copy(out, ptmx) }() // The copy ends when the terminal closes.

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait() // The exit code below shows the outcome.

		close(exited)
	}()

	type step struct {
		input, want string
		resize      *pty.Winsize
	}

	steps := []step{
		{input: "stty size\n", want: "30 100"},
		{input: "echo $((6 * 7))\n", want: "42"},
	}

	if runtime.GOOS == "linux" {
		steps = append(steps, step{input: "stty size\n", want: "40 120", resize: &pty.Winsize{Rows: 40, Cols: 120}})
	}

	for _, step := range steps {
		if step.resize != nil {
			err = pty.Setsize(ptmx, step.resize)
			if err != nil {
				t.Fatal(err)
			}
		}

		// After a resize the input repeats until the output appears, because the resize reaches the guest a moment later.
		for sent := false; !strings.Contains(out.String(), step.want); {
			if !sent || step.resize != nil {
				_, err = io.WriteString(ptmx, step.input)
				if err != nil {
					t.Fatal(err)
				}

				sent = true
			}

			select {
			case <-ctx.Done():
				t.Fatalf("after %q, output %q does not contain %q", step.input, out.String(), step.want)
			case <-exited:
				t.Fatalf("sbxr exited before the output %q contained %q", out.String(), step.want)
			case <-time.After(time.Second):
			}
		}
	}

	_, err = io.WriteString(ptmx, "exit 7\n")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("sbxr did not exit")
	}

	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Errorf("exit code = %d, want 7 (output %q)", code, out.String())
	}
}

// syncBuffer is a buffer that one goroutine writes while another reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // A buffer write does not fail.
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// sbxr runs bin and returns the combined output and the exit code.
func sbxr(t *testing.T, bin string, environ []string, args ...string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = environ

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}

	return string(out), cmd.ProcessState.ExitCode()
}

// hvCacheDir returns a cache directory for one test.
// On Linux, cache files belong to subordinate ids, so a cleanup prunes the cache before the temporary directory goes.
func hvCacheDir(t *testing.T) string {
	t.Helper()

	bin := hvBinary(t)
	dir := filepath.Join(t.TempDir(), "cache")

	t.Cleanup(func() {
		// Only image work, which runs inside the user namespace, creates the images directory.
		if _, err := os.Stat(filepath.Join(dir, "images")); err != nil {
			return
		}

		cmd := exec.CommandContext(context.Background(), bin, "prune", "--force")
		cmd.Env = hvEnv(dir)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("prune %s: %v: %s", dir, err, out)
		}
	})

	return dir
}

// hvEnv returns the environment for sbxr: PATH, SBXR_LIB_DIR when set, and the cache directory when given.
func hvEnv(cacheDir ...string) []string {
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
	hvBinOnce sync.Once
	hvBin     string
	errHVBin  error

	hvImagesOnce sync.Once
	hvRefs       map[string]string
	errHVImages  error
)

// hvBinary returns the sbxr binary under test: SBXR_BIN when set, else a signed release build.
// On Ubuntu with the AppArmor restriction, pass SBXR_BIN=/usr/local/bin/sbxr with the profile installed.
func hvBinary(t *testing.T) string {
	t.Helper()

	scenario.RequireHypervisor(t)

	hvBinOnce.Do(func() {
		hvBin = os.Getenv("SBXR_BIN")
		if hvBin != "" {
			return
		}

		dir, err := os.MkdirTemp("", "sbxr-hv-") //nolint:usetesting // The binary outlives the first test.
		if err != nil {
			errHVBin = err

			return
		}

		hvBin = filepath.Join(dir, "sbxr")
		errHVBin = scenario.Build(context.Background(), hvBin)

		if errHVBin == nil && runtime.GOOS == "darwin" {
			out, err := exec.CommandContext(context.Background(), "codesign", "--force", "--sign", "-", "--entitlements", "../../entitlements.plist", hvBin).CombinedOutput()
			if err != nil {
				errHVBin = errors.New("codesign: " + string(out))
			}
		}
	})

	if errHVBin != nil {
		t.Fatal(errHVBin)
	}

	return hvBin
}

// hvImages serves the scenario images built on hvBase for the rest of the test process.
func hvImages(t *testing.T) map[string]string {
	t.Helper()

	hvImagesOnce.Do(func() {
		ref, err := name.ParseReference(hvBase)
		if err != nil {
			errHVImages = err

			return
		}

		base, err := remote.Image(ref, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
		if err != nil {
			errHVImages = err

			return
		}

		hvRefs, _, errHVImages = scenario.ServeImages(context.Background(), base)
	})

	if errHVImages != nil {
		t.Fatal(errHVImages)
	}

	return hvRefs
}

// TestHVVolumeSymlinksCannotEscape mounts a volume through symlinks in an earlier volume that lead to a reserved path,
// the guest root, or through a file.
func TestHVVolumeSymlinksCannotEscape(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	refs := hvImages(t)
	cacheDir := hvCacheDir(t)

	links, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(links, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, to := range map[string]string{"escape": "/missing/../proc", "root": "/", "edge": "/a/file/../ok"} {
		if err := os.Symlink(to, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}

	for name, want := range map[string]string{"escape": "reserved path /proc", "root": "resolves to the guest root", "edge": "not a directory"} {
		args := append(append([]string{"run", "-q"}, hvFlags...), "-v", links+":/a", "-v", target+":/a/"+name, refs[scenario.Busybox], "true")

		out, code := sbxr(t, bin, hvEnv(cacheDir), args...)
		if code != 125 || !strings.Contains(out, want) {
			t.Errorf("volume at /a/%s: sbxr run = %d, %q; want 125 and %q", name, code, out, want)
		}
	}

	if err := scenario.CheckClean(cacheDir); err != nil {
		t.Error(err)
	}
}
