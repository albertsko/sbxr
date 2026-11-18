package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/scenario"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

var (
	bin string
	// releaseBin is the release build of cmd/sbxr, and bin is the fake build.
	releaseBin string
	// busybox is the reference of the scenario busybox image in a registry in this process.
	busybox string
)

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

	err = scenario.Build(context.Background(), bin, scenario.FakePkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	releaseBin = filepath.Join(dir, "sbxr-release")

	err = scenario.Build(context.Background(), releaseBin, scenario.ReleasePkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	base, err := fake.Image()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	refs, stop, err := scenario.ServeImages(context.Background(), base)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer stop()

	busybox = refs[scenario.Busybox]

	return m.Run()
}

// env returns a minimal environment with a private cache, plus extra.
func env(t *testing.T, extra ...string) []string {
	t.Helper()

	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"SBXR_CACHE_DIR=" + filepath.Join(t.TempDir(), "cache"),
	}, extra...)
}

// cacheDirOf returns the SBXR_CACHE_DIR value of environ.
func cacheDirOf(environ []string) string {
	for _, kv := range environ {
		if dir, ok := strings.CutPrefix(kv, "SBXR_CACHE_DIR="); ok {
			return dir
		}
	}

	return ""
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
		{name: "failed preflight", args: []string{"run", busybox}, extraEnv: []string{"SBXR_FAKE_PREFLIGHT_FAIL=hypervisor"}, wantCode: 125, want: `sbxr: preflight "hypervisor"`},
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
	out, code, _ := sbxrOnTerminal(t, env(t), "run", "-q", busybox, "echo", "hi")
	if code != 0 || out != "hi\r\n" {
		t.Errorf("sbxr run = %d, %q; want 0, %q", code, out, "hi\r\n")
	}
}

func TestPullProgressOnlyOnTerminalWithoutQuiet(t *testing.T) {
	t.Parallel()

	out, code, _ := sbxrOnTerminal(t, env(t), "run", busybox, "true")
	if code != 0 || !strings.Contains(out, "Pulling "+busybox+": layer ") {
		t.Errorf("sbxr run on a terminal = %d, %q; want 0 and pull progress", code, out)
	}

	out, code, _ = sbxrOnTerminal(t, env(t), "run", "-q", busybox, "true")
	if code != 0 || out != "" {
		t.Errorf("sbxr run -q on a terminal = %d, %q; want 0 and no output", code, out)
	}
}

func TestTTYGetsTerminalSize(t *testing.T) {
	t.Parallel()

	// Both the guest terminal and the test terminal turn a newline into CR LF.
	out, code, _ := sbxrOnTerminal(t, env(t), "run", "-q", "-t", busybox, "stty", "size")
	if code != 0 || out != "30 100\r\r\n" {
		t.Errorf("sbxr run -t stty size = %d, %q; want 0, %q", code, out, "30 100\r\r\n")
	}
}

func TestColorsOnlyOnTerminalWithoutNoColor(t *testing.T) {
	t.Parallel()

	runError := []string{"run", "-w", "app", busybox}

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

	out, code, _ := sbxrOnTerminal(t, env(t, "NO_COLOR=1"), "run", "-w", "app", busybox)

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
		{name: "workload exit code prints nothing", args: []string{"run", busybox, "sh", "-c", "exit 3"}, wantCode: 3},
		{name: "flags after the image belong to the workload", args: []string{"run", busybox, "echo", "--debug", "-e", "x"}, wantStdout: "--debug -e x\n"},
		{name: "separator after the image", args: []string{"run", busybox, "--", "echo", "hi"}, wantStdout: "hi\n"},
		{name: "control character in argument", args: []string{"run", busybox, "echo", "a\x1bb"}, wantCode: 125, wantStderr: `sbxr: argument 2 "a\x1bb": control character U+001B; only tab and newline are allowed` + "\n"},
		{name: "reserved env", args: []string{"run", "-e", "KRUN_INIT=/x", busybox}, wantCode: 125, wantStderr: `sbxr: -e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable` + "\n"},
		{name: "publish without net", args: []string{"run", "-p", "8080:80", busybox}, wantCode: 125, wantStderr: `sbxr: -p "8080:80": requires --net` + "\n"},
		{name: "interactive terminal without a terminal", args: []string{"run", "-it", busybox, "true"}, wantCode: 125, wantStderr: `sbxr: -t: the input device is not a terminal; drop -t, or run sbxr from a terminal` + "\n"},
		{name: "failed preflight", args: []string{"run", busybox, "true"}, extraEnv: []string{"SBXR_FAKE_PREFLIGHT_FAIL=libkrun"}, wantCode: 125, wantStderr: `sbxr: preflight "libkrun": fake libkrun failure; fake fix for libkrun` + "\n"},
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

// TestDoctorSetupBlock checks that a failed doctor report ends with the Linux setup block on Linux only.
func TestDoctorSetupBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fail string
		want bool
	}{
		{name: "failed check", fail: "hypervisor", want: runtime.GOOS == "linux"},
		{name: "all checks pass", fail: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := sbxr(t, env(t, "SBXR_FAKE_PREFLIGHT_FAIL="+tt.fail), "doctor")
			if got := strings.HasSuffix(res.stdout, "\n"+preflight.LinuxSetup); got != tt.want {
				t.Errorf("stdout %q ends with the setup block: %v, want %v", res.stdout, got, tt.want)
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

// TestDoctorJSONMatchesSchema checks doctor reports against docs/doctor.schema.json.
// It also checks that the release build runs the doctor checks of docs/cli.md in order.
func TestDoctorJSONMatchesSchema(t *testing.T) {
	t.Parallel()

	schema, err := jsonschema.NewCompiler().Compile("../../docs/doctor.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	platformCheck := map[string]string{"darwin": "entitlement", "linux": "userns"}[runtime.GOOS]
	wantIDs := []string{"hypervisor", "libkrun", "libkrunfw", platformCheck, "cache"}

	tests := []struct {
		name    string
		bin     string
		environ []string
	}{
		{name: "fake", bin: bin, environ: env(t, "SBXR_FAKE_PREFLIGHT_FAIL=hypervisor")},
		{name: "release", bin: releaseBin, environ: env(t)},
		{name: "release without libraries", bin: releaseBin, environ: env(t, "SBXR_LIB_DIR="+t.TempDir())},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), tt.bin, "doctor", "--json")
			cmd.Env = tt.environ

			out, err := cmd.Output()
			if _, isExit := errors.AsType[*exec.ExitError](err); err != nil && !isExit {
				t.Fatal(err)
			}

			report, err := jsonschema.UnmarshalJSON(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}

			err = schema.Validate(report)
			if err != nil {
				t.Errorf("doctor --json %s does not match the schema: %v", out, err)
			}

			var ids []string

			for _, c := range report.(map[string]any)["checks"].([]any) { //nolint:forcetypeassert // The schema guarantees the shape.
				ids = append(ids, c.(map[string]any)["id"].(string)) //nolint:forcetypeassert // The schema guarantees the shape.
			}

			if !slices.Equal(ids, wantIDs) {
				t.Errorf("check IDs %q, want %q", ids, wantIDs)
			}
		})
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
			cacheDir := cacheDirOf(environ)

			cmd := exec.CommandContext(t.Context(), bin, "run", busybox, "sh", "-c", "echo ready; sleep 30")
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

	tm := startOnTerminal(t, env(t), nil, "run", "-it", busybox, "sh", "-c", "echo ready; sleep 30")
	tm.waitForOutput("ready")
	tm.write("\x03")

	out, code, _ := tm.wait()
	if code != 130 || strings.Contains(out, "sbxr:") {
		t.Errorf("sbxr run -it then Ctrl-C = %d, %q; want 130 and no sbxr error", code, out)
	}
}

func TestCtrlCRightAfterRawModeStopsTheWorkload(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, env(t), nil, "run", "-it", busybox, "sleep", "30")

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

	tm := startOnTerminal(t, env(t), nil, "run", "-it", busybox, "sh", "-c", "echo ready; sleep 30")
	tm.waitForOutput("ready")
	tm.write(strings.Repeat("x", 100))
	tm.write("\x03")

	if _, code, _ := tm.wait(); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func TestClosedTerminalStopsTheRun(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	tm := startOnTerminal(t, environ, nil, "run", busybox, "sh", "-c", "echo ready; sleep 30")
	tm.waitForOutput("ready")

	// A closed terminal sends SIGHUP to its foreground process group, which holds the runner and the VM process.
	// The test sends it directly, because the output reader of the test keeps the terminal from hanging up.
	// Diagnostics for a macOS CI flake that kills the test task with SIGHUP: the signal must reach only the run's group.
	pid := tm.cmd.Process.Pid

	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid {
		t.Fatalf("run pid %d has process group %d (%v); want its own group", pid, pgid, err)
	}

	err = syscall.Kill(-pid, syscall.SIGHUP)
	if err != nil {
		t.Fatal(err)
	}

	if _, code, _ := tm.wait(); code != 129 {
		t.Errorf("exit code = %d, want 129", code)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestCtrlCEndsAStalledDrainAfterTheVMExits(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)
	word := strings.Repeat("x", 100000)

	// Nobody reads this pipe, so the output copy stalls once the pipe buffer is full.
	stdout, unread, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer unread.Close()

	tm := startOnTerminal(t, environ, unread, "run", "-it", busybox, "echo", word, word)

	for deadline := time.Now().Add(10 * time.Second); !tm.raw(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal never became raw")
		}
	}

	// The workload blocks on the full output until the first Ctrl-C ends it in the guest. The stalled drain then keeps the runner waiting.
	time.Sleep(300 * time.Millisecond)
	tm.write("\x03")

	for deadline := time.Now().Add(10 * time.Second); tm.raw(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal stayed raw after the VM exited")
		}
	}

	// The terminal is back in cooked mode, so the second Ctrl-C is a SIGINT to the runner.
	tm.write("\x03")

	done := make(chan int, 1)

	go func() {
		_, code, _ := tm.wait()
		done <- code
	}()

	select {
	case code := <-done:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	case <-time.After(10 * time.Second):
		_ = tm.cmd.Process.Kill()

		t.Fatal("sbxr ignored Ctrl-C while the output reader stalled")
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestNextRunRemovesStaleRunState(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)
	runner, vmPID := startStoppedVM(t, environ)

	// Killing the runner and the stopped VM process leaves no process that could clean up.
	for _, pid := range []int{runner.Process.Pid, vmPID} {
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err != nil {
			t.Fatal(err)
		}
	}

	_ = runner.Wait() // The process was killed.

	waitForNoProcess(t, cacheDir)

	if scenario.CheckClean(cacheDir) == nil {
		t.Fatal("no run state remains after SIGKILL, so the test checks nothing")
	}

	if res := sbxr(t, environ, "run", busybox, "true"); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	err := scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestLiveVMKeepsItsRunState checks that a VM process that outlives its runner keeps its run root and the cache.
func TestLiveVMKeepsItsRunState(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)
	runner, vmPID := startStoppedVM(t, environ)

	// The stopped VM process cannot see its lifeline close, so it stays alive with its run state.
	err := syscall.Kill(runner.Process.Pid, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	_ = runner.Wait() // The process was killed.

	runs, err := filepath.Glob(filepath.Join(cacheDir, "runs", "*", "root"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("run roots = %q, %v; want the root of the stopped VM", runs, err)
	}

	if res := sbxr(t, environ, "run", busybox, "true"); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	if res := sbxr(t, environ, "prune", "--force"); res.code != 1 || !strings.Contains(res.stderr, "a running sbxr run uses the image cache") {
		t.Errorf("prune = %d, stderr %q; want 1 and a busy cache", res.code, res.stderr)
	}

	if _, err := os.Stat(runs[0]); err != nil {
		t.Fatalf("the root of the stopped VM is gone: %v", err)
	}

	// Resumed, the VM process sees the closed lifeline and removes its run state.
	err = syscall.Kill(vmPID, syscall.SIGCONT)
	if err != nil {
		t.Fatal(err)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// startStoppedVM starts a run, waits until its workload runs, and stops the VM process with SIGSTOP.
// It returns the runner and the VM process ID.
func startStoppedVM(t *testing.T, environ []string) (*exec.Cmd, int) {
	t.Helper()

	cacheDir := cacheDirOf(environ)

	cmd := exec.CommandContext(t.Context(), bin, "run", busybox, "sh", "-c", "echo ready; sleep 30")
	cmd.Env = environ

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	_, err = io.ReadFull(stdout, make([]byte, len("ready\n")))
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", "__vm "+cacheDir).Output()
	if err != nil {
		t.Fatalf("find the VM process: %v", err)
	}

	pid, err := strconv.Atoi(strings.Fields(string(out))[0])
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	err = syscall.Kill(pid, syscall.SIGSTOP)
	if err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		state, _ := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
			return cmd, pid
		}

		if time.Now().After(deadline) {
			t.Fatalf("the VM process is not stopped: state %q", state)
		}
	}
}

func TestStderrOnTerminalWithPipedStdout(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer

	// cat writes its error to stderr.
	out, code, _ := startOnTerminal(t, env(t), &stdout, "run", "-q", busybox, "sh", "-c", "echo OUT; cat /missing").wait()
	if code != 1 || stdout.String() != "OUT\n" || !strings.Contains(out, "/missing") || strings.Contains(out, "OUT") {
		t.Errorf("sbxr run = %d, stdout %q, terminal %q; want 1, OUT on stdout, and the cat error on the terminal", code, stdout.String(), out)
	}
}

func TestGoneStdoutReaderStopsTheRun(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	cmd := exec.CommandContext(t.Context(), bin, "run", busybox, "yes")
	cmd.Env = environ

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Like head -1, the reader takes one line and goes away.
	_, err = bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}

	_ = stdout.Close()

	done := make(chan struct{})

	go func() {
		_ = cmd.Wait() // The exit code below shows the outcome.

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()

		t.Fatal("sbxr kept running after its stdout reader went away")
	}

	if code := cmd.ProcessState.ExitCode(); code != 141 || stderr.Len() > 0 {
		t.Errorf("sbxr run yes = %d, stderr %q; want 141 and no sbxr error", code, stderr.String())
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestGoneStdoutReaderStopsATTYRun(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	tm := startOnTerminal(t, environ, pw, "run", "-q", "-t", busybox, "yes")
	_ = pw.Close()

	lines := bufio.NewReader(pr)

	for range 5 {
		_, err = lines.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
	}

	_ = pr.Close()

	if code := waitExit(t, tm.cmd); code != 141 || strings.Contains(tm.output(), "sbxr:") {
		t.Errorf("sbxr run -t yes = %d, terminal %q; want 141 and no sbxr error", code, tm.output())
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestGoneReaderOfDebugOutputStopsTheRun closes a pipe that carries both stdout and the debug logs on stderr.
// A later log write must not kill the runner before it cleans up.
func TestGoneReaderOfDebugOutputStopsTheRun(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), bin, "--debug", "run", busybox, "yes")
	cmd.Env = environ
	cmd.Stdout, cmd.Stderr = pw, pw

	err = cmd.Start()
	_ = pw.Close()

	if err != nil {
		t.Fatal(err)
	}

	lines := bufio.NewReader(pr)

	for range 5 {
		_, err = lines.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
	}

	_ = pr.Close()

	if code := waitExit(t, cmd); code != 141 {
		t.Errorf("exit code = %d, want 141", code)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestReaderGoneAfterCtrlCWithStalledOutput closes a stalled stdout reader after Ctrl-C, while an output copy still blocks.
func TestReaderGoneAfterCtrlCWithStalledOutput(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), bin, "run", busybox, "yes")
	cmd.Env = environ
	cmd.Stdout = pw

	err = cmd.Start()
	_ = pw.Close()

	if err != nil {
		t.Fatal(err)
	}

	// The first line proves that the runner handles signals. Then nobody reads, so the output copy blocks on the full pipe.
	_, err = bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Process.Signal(syscall.SIGINT)
	if err != nil {
		t.Fatal(err)
	}

	// The reader goes away after the runner handled the signal. The wait is best effort, an earlier close ends with 141.
	time.Sleep(100 * time.Millisecond)

	_ = pr.Close()

	if code := waitExit(t, cmd); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// waitExit waits at most 10 seconds for cmd and returns its exit code.
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

func TestKilledVMProcessIsAnSbxrError(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)

	cmd := exec.CommandContext(t.Context(), bin, "run", busybox, "sh", "-c", "echo ready; sleep 30")
	cmd.Env = environ

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	_, err = io.ReadFull(stdout, make([]byte, len("ready\n")))
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", "__vm "+cacheDir).Output()
	if err != nil {
		t.Fatalf("find the VM process: %v", err)
	}

	pid, err := strconv.Atoi(strings.Fields(string(out))[0])
	if err != nil {
		t.Fatal(err)
	}

	err = syscall.Kill(pid, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	_ = cmd.Wait() // The exit code below shows the outcome.

	want := "sbxr: vm: the VM process was killed by signal 9; run again with --debug and report a bug\n"
	if code := cmd.ProcessState.ExitCode(); code != 125 || stderr.String() != want {
		t.Errorf("sbxr run = %d, stderr %q; want 125 and %q", code, stderr.String(), want)
	}

	waitForNoProcess(t, cacheDir)

	err = scenario.CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
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

	for _, args := range [][]string{{"run", busybox, "echo", "hi"}, {"run", "-t", busybox, "echo", "hi"}} {
		out, code, _ := startOnTerminalWith(t, env(t), nil, tostop, args...).wait()
		if code != 0 || !strings.Contains(out, "hi") {
			t.Errorf("sbxr %q = %d, %q; want 0 and hi", args, code, out)
		}
	}
}

func TestSIGTERMWithAStalledTTYReader(t *testing.T) {
	t.Parallel()

	environ := env(t)
	cacheDir := cacheDirOf(environ)
	word := strings.Repeat("x", 100000)

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", busybox, "echo", word, word)
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

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", busybox, "cat")
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

	cmd := exec.CommandContext(t.Context(), bin, "run", "-t", busybox, "echo", word, word, word, word)
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

	tm := startOnTerminal(t, env(t), &stdout, "run", "-w", "app", busybox)

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
