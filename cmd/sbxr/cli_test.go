package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/albertsko/sbxr/internal/scenario"
)

var bin string

func TestMain(m *testing.M) {
	os.Exit(buildAndRun(m))
}

func buildAndRun(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sbxr-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer os.RemoveAll(dir)

	bin = filepath.Join(dir, "sbxr")

	err = scenario.Build(context.Background(), bin, "sbxrfake")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	return m.Run()
}

// env returns a minimal environment with private fake images and cache, plus extra.
func env(t *testing.T, extra ...string) []string {
	t.Helper()

	dir := t.TempDir()

	err := scenario.WriteFakeImages(filepath.Join(dir, "images"))
	if err != nil {
		t.Fatal(err)
	}

	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"SBXR_FAKE_IMAGES=" + filepath.Join(dir, "images"),
		"SBXR_CACHE_DIR=" + filepath.Join(dir, "cache"),
	}, extra...)
}

type result struct {
	stdout, stderr string
	code           int
}

// sbxr runs the binary with stdout and stderr on pipes.
func sbxr(t *testing.T, environ []string, args ...string) result {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Env = environ

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}

	return result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}

// sbxrOnTerminal runs the binary on a new pseudo-terminal that never answers terminal queries.
// It returns the terminal output, the exit code, and the run time.
func sbxrOnTerminal(t *testing.T, environ []string, args ...string) (string, int, time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)

	cmd.Env = append(slices.Clone(environ), "TERM=xterm-256color")

	start := time.Now()

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	var out bytes.Buffer

	copied := make(chan struct{})

	go func() {
		defer close(copied)

		_, _ = io.Copy(&out, ptmx) // The copy ends with an I/O error when the terminal closes.
	}()

	err = cmd.Wait()
	elapsed := time.Since(start)

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}

	ptmx.Close()
	<-copied

	return out.String(), cmd.ProcessState.ExitCode(), elapsed
}

func TestHelpAndErrorsDoNotWaitForTerminalQueries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		extraEnv []string
		wantCode int
		want     string
	}{
		{name: "help", args: []string{"--help"}, wantCode: 0, want: "doctor"},
		{name: "run help", args: []string{"run", "--help"}, wantCode: 0, want: "--allow-lan"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, want: `sbxr: unknown command "nope"`},
		{name: "bad flag", args: []string{"run", "--nope"}, wantCode: 125, want: "sbxr: unknown flag: --nope"},
		{name: "bad doctor flag", args: []string{"doctor", "--nope"}, wantCode: 2, want: "sbxr: unknown flag: --nope"},
		{name: "failed preflight", args: []string{"run", scenario.Busybox}, extraEnv: []string{"SBXR_FAKE_PREFLIGHT_FAIL=hypervisor"}, wantCode: 125, want: `sbxr: preflight "hypervisor"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, code, elapsed := sbxrOnTerminal(t, env(t, tt.extraEnv...), tt.args...)
			if elapsed >= time.Second {
				t.Errorf("sbxr %q took %v, want under 1s", tt.args, elapsed)
			}

			if code != tt.wantCode || !strings.Contains(ansi.Strip(out), tt.want) {
				t.Errorf("sbxr %q = %d, %q; want %d and output with %q", tt.args, code, out, tt.wantCode, tt.want)
			}
		})
	}
}

func TestWorkloadOutputReachesTerminalStdout(t *testing.T) {
	t.Parallel()

	out, code, elapsed := sbxrOnTerminal(t, env(t), "run", scenario.Busybox, "echo", "hi")
	if code != 0 || out != "hi\r\n" {
		t.Errorf("sbxr run = %d, %q; want 0, %q", code, out, "hi\r\n")
	}

	if elapsed >= time.Second {
		t.Errorf("sbxr run took %v, want under 1s", elapsed)
	}
}

func TestTTYGetsTerminalSize(t *testing.T) {
	t.Parallel()

	// Both the guest terminal and the test terminal turn a newline into CR LF.
	out, code, _ := sbxrOnTerminal(t, env(t), "run", "-t", scenario.Busybox, "stty", "size")
	if code != 0 || out != "30 100\r\r\n" {
		t.Errorf("sbxr run -t stty size = %d, %q; want 0, %q", code, out, "30 100\r\r\n")
	}
}

func TestColorsOnlyOnTerminalWithoutNoColor(t *testing.T) {
	t.Parallel()

	runError := []string{"run", "-w", "app", scenario.Busybox}

	tests := []struct {
		name     string
		extraEnv []string
		args     []string
		wantANSI bool
	}{
		{name: "error", args: runError, wantANSI: true},
		{name: "error with NO_COLOR", extraEnv: []string{"NO_COLOR=1"}, args: runError},
		{name: "error with --no-color", args: append([]string{"--no-color"}, runError...)},
		{name: "help", args: []string{"run", "--help"}, wantANSI: true},
		{name: "help with NO_COLOR", extraEnv: []string{"NO_COLOR=1"}, args: []string{"run", "--help"}},
		{name: "help with --no-color", args: []string{"run", "--help", "--no-color"}},
		{name: "doctor with --no-color", extraEnv: []string{"SBXR_FAKE_PREFLIGHT_FAIL=cache"}, args: []string{"doctor", "--no-color"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, _, _ := sbxrOnTerminal(t, env(t, tt.extraEnv...), tt.args...)
			if strings.Contains(out, "\x1b") != tt.wantANSI {
				t.Errorf("output %q, want ANSI codes: %v", out, tt.wantANSI)
			}
		})
	}
}

func TestErrorIsOneLine(t *testing.T) {
	t.Parallel()

	out, code, _ := sbxrOnTerminal(t, env(t, "NO_COLOR=1"), "run", "-w", "app", scenario.Busybox)

	want := "sbxr: -w \"app\": must be absolute; for example /app\r\n"
	if code != 125 || out != want {
		t.Errorf("sbxr run -w app = %d, %q; want 125, %q", code, out, want)
	}
}

func TestPipedOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		extraEnv   []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no arguments print usage", args: nil, wantStdout: "sbxr run --net "},
		{name: "workload exit code prints nothing", args: []string{"run", scenario.Busybox, "sh", "-c", "exit 3"}, wantCode: 3},
		{name: "flags after the image belong to the workload", args: []string{"run", scenario.Busybox, "echo", "--debug", "-e", "x"}, wantStdout: "--debug -e x\n"},
		{name: "separator after the image", args: []string{"run", scenario.Busybox, "--", "echo", "hi"}, wantStdout: "hi\n"},
		{name: "control character in argument", args: []string{"run", scenario.Busybox, "echo", "a\x1bb"}, wantCode: 125, wantStderr: `sbxr: argument 2 "a\x1bb": control character U+001B; only tab and newline are allowed` + "\n"},
		{name: "reserved env", args: []string{"run", "-e", "KRUN_INIT=/x", scenario.Busybox}, wantCode: 125, wantStderr: `sbxr: -e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable` + "\n"},
		{name: "publish without net", args: []string{"run", "-p", "8080:80", scenario.Busybox}, wantCode: 125, wantStderr: `sbxr: -p "8080:80": requires --net` + "\n"},
		{name: "failed preflight", args: []string{"run", scenario.Busybox, "true"}, extraEnv: []string{"SBXR_FAKE_PREFLIGHT_FAIL=libkrun"}, wantCode: 125, wantStderr: `sbxr: preflight "libkrun": fake libkrun failure; fake fix for libkrun` + "\n"},
		{name: "bad log level", args: []string{"doctor"}, extraEnv: []string{"SBXR_LOG_LEVEL=loud"}, wantCode: 1, wantStderr: `sbxr: SBXR_LOG_LEVEL "loud": unknown level; use debug, info, warn, or error` + "\n"},
		{name: "doctor extra argument", args: []string{"doctor", "x"}, wantCode: 2, wantStderr: `sbxr: unexpected argument "x"; see 'sbxr doctor --help'` + "\n"},
		{name: "apparmor profile", args: []string{"doctor", "apparmor-profile"}, wantStdout: "profile sbxr /{usr/bin,usr/local/bin}/sbxr flags=(unconfined) {"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := sbxr(t, env(t, tt.extraEnv...), tt.args...)
			if res.code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", res.code, tt.wantCode, res.stderr)
			}

			if !strings.Contains(res.stdout, tt.wantStdout) || tt.wantStdout == "" && res.stdout != "" {
				t.Errorf("stdout = %q, want %q", res.stdout, tt.wantStdout)
			}

			if res.stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", res.stderr, tt.wantStderr)
			}
		})
	}
}

func TestDoctorJSON(t *testing.T) {
	t.Parallel()

	res := sbxr(t, env(t, "SBXR_FAKE_PREFLIGHT_FAIL=hypervisor"), "doctor", "--json")
	if res.code != 1 {
		t.Errorf("exit code = %d, want 1", res.code)
	}

	var report struct {
		SchemaVersion int  `json:"schemaVersion"`
		OK            bool `json:"ok"`
		Checks        []map[string]string
	}

	err := json.Unmarshal([]byte(res.stdout), &report)
	if err != nil {
		t.Fatalf("stdout %q: %v", res.stdout, err)
	}

	if report.SchemaVersion != 1 || report.OK || len(report.Checks) == 0 {
		t.Fatalf("report = %+v, want schema 1, not ok, with checks", report)
	}

	for _, c := range report.Checks {
		_, hasFix := c["fix"]
		if wantFail := c["id"] == "hypervisor"; (c["status"] == "fail") != wantFail || hasFix != wantFail || c["detail"] == "" {
			t.Errorf("check %v, want status fail with a fix only for hypervisor", c)
		}
	}
}

func TestSignals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		signal   syscall.Signal
		wantCode int
	}{
		{name: "SIGINT", signal: syscall.SIGINT, wantCode: 130},
		{name: "SIGTERM", signal: syscall.SIGTERM, wantCode: 143},
		{name: "SIGKILL", signal: syscall.SIGKILL, wantCode: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			environ := env(t)
			cacheDir := strings.TrimPrefix(environ[2], "SBXR_CACHE_DIR=")

			cmd := exec.CommandContext(t.Context(), bin, "run", scenario.Busybox, "sh", "-c", "echo ready; sleep 30")
			cmd.Env = environ

			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}

			err = cmd.Start()
			if err != nil {
				t.Fatal(err)
			}

			ready := make([]byte, len("ready\n"))

			_, err = io.ReadFull(stdout, ready)
			if err != nil {
				t.Fatal(err)
			}

			err = cmd.Process.Signal(tt.signal)
			if err != nil {
				t.Fatal(err)
			}

			_ = cmd.Wait() // The exit code below shows the outcome.

			if code := cmd.ProcessState.ExitCode(); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}

			waitForNoProcess(t, cacheDir)
		})
	}
}

// waitForNoProcess waits until no process mentions dir. A killed parent cannot remove its run state.
func waitForNoProcess(t *testing.T, dir string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
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

		time.Sleep(50 * time.Millisecond)
	}
}
