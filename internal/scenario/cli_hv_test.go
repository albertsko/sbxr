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
			CacheDir: filepath.Join(t.TempDir(), "cache"),
			Images:   refs,
			Flags:    hvFlags,
			Env:      hvEnv(),
			Caps:     []scenario.Capability{scenario.Stdin, scenario.TTY, scenario.SplitStderr, scenario.SignalExit},
		}
	})
}

func TestHVDoctorPasses(t *testing.T) {
	t.Parallel()

	bin := hvBinary(t)
	out, code := sbxr(t, bin, hvEnv(t.TempDir()), "doctor", "--json")

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
	dir := t.TempDir()
	environ := append(hvEnv(dir), "SBXR_LIB_DIR="+filepath.Join(dir, "nolib"))

	out, code := sbxr(t, bin, environ, "run", refs[scenario.Busybox], "echo", "hi")
	if code != 125 || !strings.HasPrefix(out, `sbxr: preflight "libkrun": `) {
		t.Errorf("sbxr run = %d, %q; want 125 and a libkrun preflight error", code, out)
	}

	err := scenario.CheckClean(filepath.Join(dir, "cache"))
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

	out, code := sbxr(t, bin, hvEnv(t.TempDir()), "doctor")
	if code != 1 || !strings.Contains(out, "fail  userns") || !strings.Contains(out, "install sbxr to /usr/local/bin") || strings.Contains(out, "apparmor_parser") {
		t.Errorf("sbxr doctor = %d, %q; want a userns failure that asks for /usr/local/bin and prints no profile command", code, out)
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

	cmd.Env = append(hvEnv(t.TempDir()), "TERM=xterm")
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

// hvEnv returns the environment for sbxr: PATH, SBXR_LIB_DIR when set, and a cache in dir when given.
func hvEnv(dir ...string) []string {
	environ := []string{"PATH=" + os.Getenv("PATH")}

	if lib := os.Getenv("SBXR_LIB_DIR"); lib != "" {
		environ = append(environ, "SBXR_LIB_DIR="+lib)
	}

	if len(dir) > 0 {
		environ = append(environ, "SBXR_CACHE_DIR="+filepath.Join(dir[0], "cache"))
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
