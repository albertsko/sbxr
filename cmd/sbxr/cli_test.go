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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"

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

	return startOnTerminal(t, environ, nil, args...).wait()
}

// terminal is a run of the binary on a pseudo-terminal of 30 rows and 100 columns.
type terminal struct {
	t     *testing.T
	ptmx  *os.File
	tty   *os.File
	cmd   *exec.Cmd
	start time.Time

	mu  sync.Mutex
	out bytes.Buffer
}

// startOnTerminal starts the binary with stdin, stderr, and stdout on a new pseudo-terminal.
// A non-nil stdout replaces the terminal as stdout.
func startOnTerminal(t *testing.T, environ []string, stdout io.Writer, args ...string) *terminal {
	t.Helper()

	return startOnTerminalWith(t, environ, stdout, nil, args...)
}

// startOnTerminalWith is startOnTerminal with a setup function that changes the terminal before the start.
func startOnTerminalWith(t *testing.T, environ []string, stdout io.Writer, setup func(tty *os.File), args ...string) *terminal {
	t.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}

	// macOS drops unread output when the last terminal file closes, so the test keeps tty open.
	t.Cleanup(func() {
		tty.Close()
		ptmx.Close()
	})

	err = pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}

	if setup != nil {
		setup(tty)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, bin, args...)

	cmd.Env = append(slices.Clone(environ), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if stdout != nil {
		cmd.Stdout = stdout
	}

	tm := &terminal{t: t, ptmx: ptmx, tty: tty, cmd: cmd, start: time.Now()}

	go func() {
		buf := make([]byte, 4096)

		for {
			n, err := ptmx.Read(buf)

			tm.mu.Lock()
			tm.out.Write(buf[:n])
			tm.mu.Unlock()

			if err != nil {
				return
			}
		}
	}()

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	return tm
}

func (tm *terminal) output() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	return tm.out.String()
}

// raw reports whether the terminal has line editing off.
func (tm *terminal) raw() bool {
	termios, err := unix.IoctlGetTermios(int(tm.tty.Fd()), getTermios)

	return err == nil && termios.Lflag&unix.ICANON == 0
}

// waitForOutput waits until the terminal output contains text.
func (tm *terminal) waitForOutput(text string) {
	tm.t.Helper()

	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(tm.output(), text); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			tm.t.Fatalf("output %q, want it to contain %q", tm.output(), text)
		}
	}
}

func (tm *terminal) write(s string) {
	tm.t.Helper()

	_, err := tm.ptmx.WriteString(s)
	if err != nil {
		tm.t.Fatal(err)
	}
}

// wait waits for the exit and then until the output stops growing.
func (tm *terminal) wait() (string, int, time.Duration) {
	tm.t.Helper()

	err := tm.cmd.Wait()
	elapsed := time.Since(tm.start)

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		tm.t.Fatal(err)
	}

	for last := tm.output(); ; last = tm.output() {
		time.Sleep(100 * time.Millisecond)

		if out := tm.output(); out == last {
			return out, tm.cmd.ProcessState.ExitCode(), elapsed
		}
	}
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

	// The CLI hides stdout from fang, so this also checks that run restores it.
	out, code, _ := sbxrOnTerminal(t, env(t), "run", scenario.Busybox, "echo", "hi")
	if code != 0 || out != "hi\r\n" {
		t.Errorf("sbxr run = %d, %q; want 0, %q", code, out, "hi\r\n")
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
		{name: "interactive terminal without a terminal", args: []string{"run", "-it", scenario.Busybox, "true"}, wantCode: 125, wantStderr: `sbxr: -t: the input device is not a terminal; drop -t, or run sbxr from a terminal` + "\n"},
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

			// After a SIGKILL, the VM process removes the run state when it sees the parent is gone.
			err = scenario.CheckClean(cacheDir)
			if err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCtrlCWithInteractiveTTYStopsTheWorkload(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, env(t), nil, "run", "-it", scenario.Busybox, "sh", "-c", "echo ready; sleep 30")
	tm.waitForOutput("ready")
	tm.write("\x03")

	out, code, _ := tm.wait()
	if code != 130 || strings.Contains(out, "sbxr:") {
		t.Errorf("sbxr run -it then Ctrl-C = %d, %q; want 130 and no sbxr error", code, out)
	}
}

func TestCtrlCRightAfterRawModeStopsTheWorkload(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, env(t), nil, "run", "-it", scenario.Busybox, "sleep", "30")

	// The runner sets the user's terminal raw just before it starts the VM process.
	for deadline := time.Now().Add(10 * time.Second); !tm.raw(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal never became raw")
		}
	}

	tm.write("\x03")

	if _, code, _ := tm.wait(); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func TestCtrlCReachesAWorkloadThatDoesNotReadStdin(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, env(t), nil, "run", "-it", scenario.Busybox, "sh", "-c", "echo ready; sleep 30")
	tm.waitForOutput("ready")
	tm.write(strings.Repeat("x", 100))
	tm.write("\x03")

	if _, code, _ := tm.wait(); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func TestRunWithTOSTOP(t *testing.T) {
	t.Parallel()

	// With TOSTOP, a background process group that writes to the terminal is stopped.
	tostop := func(tty *os.File) {
		termios, err := unix.IoctlGetTermios(int(tty.Fd()), getTermios)
		if err != nil {
			t.Fatal(err)
		}

		termios.Lflag |= unix.TOSTOP

		err = unix.IoctlSetTermios(int(tty.Fd()), setTermios, termios)
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"run", scenario.Busybox, "echo", "hi"}, {"run", "-t", scenario.Busybox, "echo", "hi"}} {
		out, code, _ := startOnTerminalWith(t, env(t), nil, tostop, args...).wait()
		if code != 0 || !strings.Contains(out, "hi") {
			t.Errorf("sbxr %q = %d, %q; want 0 and hi", args, code, out)
		}
	}
}

func TestSIGTERMWithAStalledTTYReader(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := strings.TrimPrefix(environ[2], "SBXR_CACHE_DIR=")
	word := strings.Repeat("x", 100000)

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", scenario.Busybox, "echo", word, word)
	cmd.Env = environ

	// Nobody reads this pipe, so the output copy stalls once the pipe buffer is full.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	// The run directory exists once the runner handles signals. Then the output fills the pipe.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		runs, _ := os.ReadDir(filepath.Join(cacheDir, "runs"))
		if len(runs) > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
	}

	time.Sleep(300 * time.Millisecond)

	err = cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait() // The exit code below shows the outcome.

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()

		t.Fatal("sbxr ignored SIGTERM while the output reader stalled")
	}

	if code := cmd.ProcessState.ExitCode(); code != 143 {
		t.Errorf("exit code = %d, want 143", code)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestTTYWithoutInteractiveNeverReadsStdin(t *testing.T) {
	t.Parallel()

	// The test keeps the pipe open, like a terminal that the user never types into.
	stdin, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keepOpen.Close()
	defer stdin.Close()

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", scenario.Busybox, "cat")
	cmd.Env = env(t)
	cmd.Stdin = stdin

	done := make(chan error, 1)

	go func() { done <- cmd.Run() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("sbxr run -t cat = %v, want exit 0", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()

		t.Fatal("sbxr run -t cat still runs, want it to read an empty stdin")
	}
}

func TestTTYOutputReachesASlowReader(t *testing.T) {
	t.Parallel()

	// The output is larger than a pipe buffer, so the runner must wait for the reader.
	word := strings.Repeat("x", 50000)
	want := strings.Join([]string{word, word, word, word}, " ") + "\r\n"

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", scenario.Busybox, "echo", word, word, word, word)
	cmd.Env = env(t)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer

	buf := make([]byte, 4096)

	for {
		time.Sleep(2 * time.Millisecond)

		n, err := stdout.Read(buf)
		got.Write(buf[:n])

		if err != nil {
			break
		}
	}

	err = cmd.Wait()
	if err != nil || got.String() != want {
		t.Errorf("sbxr run -t echo = %v with %d bytes, want exit 0 with %d bytes", err, got.Len(), len(want))
	}
}

func TestNoColorsWhenStdoutIsNotATerminal(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer

	tm := startOnTerminal(t, env(t), &stdout, "run", "-w", "app", scenario.Busybox)

	out, code, _ := tm.wait()
	if code != 125 || out != "sbxr: -w \"app\": must be absolute; for example /app\r\n" {
		t.Errorf("stderr on a terminal = %d, %q; want 125 and the error without ANSI codes", code, out)
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
