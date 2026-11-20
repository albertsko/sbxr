//go:build vm

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/preflight"
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

func TestVMDoctorPasses(t *testing.T) {
	t.Parallel()

	res := sbxr(t, vmBinary(t), vmEnv(vmCacheDir(t)), "doctor", "--json")

	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}

	err := json.Unmarshal([]byte(res.stdout), &report)
	if err != nil || res.code != 0 || !report.OK {
		t.Fatalf("sbxr doctor --json = %d, %q, %v; want 0 and ok", res.code, res.stdout, err)
	}
}

// TestVMDoctorPassesWithoutSetupBlock checks that a passing doctor report has no Linux setup block.
// TestDoctorSetupBlock checks a failed report.
func TestVMDoctorPassesWithoutSetupBlock(t *testing.T) {
	t.Parallel()

	res := sbxr(t, vmBinary(t), vmEnv(vmCacheDir(t)), "doctor")
	if res.code != 0 || strings.Contains(res.stdout, preflight.LinuxSetup) {
		t.Errorf("sbxr doctor = %d, %q; want 0 and no setup block", res.code, res.stdout)
	}
}

func TestVMFailedPreflightStartsNoVM(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)
	cacheDir := vmCacheDir(t)
	noLib := filepath.Join(t.TempDir(), "nolib")
	environ := append(vmEnv(cacheDir), "SBXR_LIB_DIR="+noLib)

	// The library file names depend on the host, so the check skips the middle of the line.
	res := sbxr(t, bin, environ, "run", refs[Busybox], "echo", "hi")

	wantStart, wantEnd := `sbxr: preflight "libkrun": `, " not found in "+noLib+"; set SBXR_LIB_DIR to the directory that holds libkrun and libkrunfw\n"
	if res.code != 125 || res.stdout != "" || !strings.HasPrefix(res.stderr, wantStart) || !strings.HasSuffix(res.stderr, wantEnd) || strings.Count(res.stderr, "\n") != 1 {
		t.Errorf("sbxr run = %d, stdout %q, stderr %q; want 125 and one line %q...%q", res.code, res.stdout, res.stderr, wantStart, wantEnd)
	}

	err := CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}

	res = sbxr(t, bin, environ, "doctor")

	_, fixAll, _ := strings.Cut(res.stdout, "Fix all:")
	if res.code != 1 || !strings.Contains(res.stdout, "fail  libkrun") || !strings.Contains(fixAll, "SBXR_LIB_DIR") {
		t.Errorf("sbxr doctor = %d, %q; want 1, a libkrun failure, and a combined fix", res.code, res.stdout)
	}
}

// TestVMAppArmorNeedsInstallPath checks the userns fix for a binary outside the profile paths.
func TestVMAppArmorNeedsInstallPath(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Skip("AppArmor does not restrict user namespaces here")
	}

	cacheDir := vmCacheDir(t)

	// The release build lives in a temporary directory, outside the profile paths.
	res := sbxr(t, releaseBinary(t), vmEnv(cacheDir), "doctor")

	// The setup block at the end installs to /usr/local/bin, so only the report above it counts.
	out, _ := strings.CutSuffix(res.stdout, preflight.LinuxSetup)
	if res.code != 1 || !strings.Contains(out, "fail  userns") || !strings.Contains(out, "install sbxr to /usr/local/bin") || strings.Contains(out, "apparmor_parser") {
		t.Errorf("sbxr doctor = %d, %q; want a userns failure that asks for /usr/local/bin and prints no profile command", res.code, res.stdout)
	}
}

func TestVMPipedOutput(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
	}{
		{name: "workload exit code prints nothing", args: []string{busybox, "sh", "-c", "exit 3"}, wantCode: 3},
		{name: "flags after the image belong to the workload", args: []string{busybox, "echo", "--debug", "-e", "x"}, wantStdout: "--debug -e x\n"},
		{name: "separator after the image", args: []string{busybox, "--", "echo", "hi"}, wantStdout: "hi\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := sbxr(t, bin, vmEnv(vmCacheDir(t)), runArgs(tt.args...)...)
			if res.code != tt.wantCode || res.stdout != tt.wantStdout || res.stderr != "" {
				t.Errorf("sbxr run = %d, stdout %q, stderr %q; want %d, %q, and no stderr", res.code, res.stdout, res.stderr, tt.wantCode, tt.wantStdout)
			}
		})
	}
}

func TestVMWorkloadOutputReachesTerminalStdout(t *testing.T) {
	t.Parallel()

	// The CLI hides stdout from fang, so this also checks that run restores it.
	out, code, _ := onTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), runArgs("-q", vmImages(t)[Busybox], "echo", "hi")...)
	if code != 0 || out != "hi\r\n" {
		t.Errorf("sbxr run = %d, %q; want 0, %q", code, out, "hi\r\n")
	}
}

func TestVMPullProgressOnlyOnTerminalWithoutQuiet(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

	out, code, _ := onTerminal(t, bin, vmEnv(vmCacheDir(t)), runArgs(busybox, "true")...)
	if code != 0 || !strings.Contains(out, "Pulling "+busybox+": layer ") {
		t.Errorf("sbxr run on a terminal = %d, %q; want 0 and pull progress", code, out)
	}

	out, code, _ = onTerminal(t, bin, vmEnv(vmCacheDir(t)), runArgs("-q", busybox, "true")...)
	if code != 0 || out != "" {
		t.Errorf("sbxr run -q on a terminal = %d, %q; want 0 and no output", code, out)
	}
}

func TestVMTTYGetsTerminalSize(t *testing.T) {
	t.Parallel()

	// Without -i, stdin is empty, so stty reads the guest terminal through /dev/tty.
	// Both the guest terminal and the test terminal turn a newline into CR LF.
	out, code, _ := onTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), runArgs("-q", "-t", vmImages(t)[Busybox], "sh", "-c", "stty size </dev/tty")...)
	if code != 0 || out != "30 100\r\r\n" {
		t.Errorf("sbxr run -t stty size = %d, %q; want 0, %q", code, out, "30 100\r\r\n")
	}
}

// TestVMStderrOnTerminalWithPipedStdout checks that each guest stream reaches its own host stream
// when only stderr is a terminal.
func TestVMStderrOnTerminalWithPipedStdout(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	term := &syncBuffer{}

	go func() { _, _ = io.Copy(term, ptmx) }() // The copy ends when the terminal closes.

	// cat writes its error to stderr and exits with 1.
	cmd := exec.CommandContext(t.Context(), bin, runArgs("-q", refs[Busybox], "sh", "-c", "echo OUT; echo ERR >&2; cat /missing")...)

	var stdout bytes.Buffer

	cmd.Env = vmEnv(vmCacheDir(t))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, &stdout, tty

	_ = cmd.Run() // The exit code below shows the outcome.

	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(term.String(), "/missing") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}

	code := cmd.ProcessState.ExitCode()
	if code != 1 || stdout.String() != "OUT\n" || !strings.Contains(term.String(), "ERR") || !strings.Contains(term.String(), "/missing") || strings.Contains(term.String(), "OUT") {
		t.Errorf("sbxr run = %d, stdout %q, terminal %q; want 1, OUT on stdout, and ERR and the cat error on the terminal", code, stdout.String(), term.String())
	}
}

// TestVMInteractiveShell types into an interactive shell on a terminal and resizes the terminal.
// libkrun 1.19.8 forwards terminal resizes to the guest only on Linux.
func TestVMInteractiveShell(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)

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

	cmd := exec.CommandContext(ctx, bin, runArgs("-it", refs[Busybox], "sh")...)

	cmd.Env = append(vmEnv(vmCacheDir(t)), "TERM=xterm")
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

func TestVMSignals(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

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

			cacheDir := vmCacheDir(t)

			cmd := exec.CommandContext(t.Context(), bin, runArgs(busybox, "sh", "-c", "echo ready; sleep 30")...)
			cmd.Env = vmEnv(cacheDir)

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
			err = CheckClean(cacheDir)
			if err != nil {
				t.Error(err)
			}
		})
	}
}

func TestVMCtrlCWithInteractiveTTYStopsTheWorkload(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), nil, nil, runArgs("-it", vmImages(t)[Busybox], "sh", "-c", "echo ready; sleep 30")...)
	tm.waitForOutput("ready")
	tm.write("\x03")

	out, code, _ := tm.wait()
	if code != 130 || strings.Contains(out, "sbxr:") {
		t.Errorf("sbxr run -it then Ctrl-C = %d, %q; want 130 and no sbxr error", code, out)
	}
}

func TestVMCtrlCRightAfterRawModeStopsTheWorkload(t *testing.T) {
	t.Parallel()

	// The guest kernel turns Ctrl-C into SIGINT, so a Ctrl-C that arrives while the VM boots never reaches the workload.
	t.Skip("known gap: a Ctrl-C while the VM boots is lost, and the workload runs on")

	tm := startOnTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), nil, nil, runArgs("-it", vmImages(t)[Busybox], "sleep", "30")...)

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

func TestVMCtrlCReachesAWorkloadThatDoesNotReadStdin(t *testing.T) {
	t.Parallel()

	tm := startOnTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), nil, nil, runArgs("-it", vmImages(t)[Busybox], "sh", "-c", "echo ready; sleep 30")...)
	tm.waitForOutput("ready")
	tm.write(strings.Repeat("x", 100))
	tm.write("\x03")

	if _, code, _ := tm.wait(); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}

func TestVMClosedTerminalStopsTheRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	tm := startOnTerminal(t, vmBinary(t), vmEnv(cacheDir), nil, nil, runArgs(vmImages(t)[Busybox], "sh", "-c", "echo ready; sleep 30")...)
	tm.waitForOutput("ready")

	// A closed terminal sends SIGHUP to its foreground process group, which holds the runner and the VM process.
	// The test sends it directly, because the output reader of the test keeps the terminal from hanging up.
	// The signal must reach only the run's group, or it stops the test process too.
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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMCtrlCEndsAStalledDrainAfterTheVMExits(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)
	word := strings.Repeat("x", 100000)

	// Nobody reads this pipe, so the output copy stalls once the pipe buffer is full.
	stdout, unread, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer unread.Close()

	tm := startOnTerminal(t, vmBinary(t), vmEnv(cacheDir), unread, nil, runArgs("-it", vmImages(t)[Busybox], "echo", word, word)...)

	for deadline := time.Now().Add(10 * time.Second); !tm.raw(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal never became raw")
		}
	}

	// The VM exits while the drain stalls. A Ctrl-C to the workload would not end it reliably,
	// because libkrun can stop reading input while its output is blocked, and the VM exits only after its output.
	waitForFullPipe(t, stdout)

	err = syscall.Kill(vmProcess(t, cacheDir), syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	// The stalled drain keeps the runner waiting after the VM exits.
	for deadline := time.Now().Add(10 * time.Second); tm.raw(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal stayed raw after the VM exited")
		}
	}

	// The terminal is back in cooked mode, so Ctrl-C is a SIGINT to the runner.
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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMNextRunRemovesStaleRunState(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]
	cacheDir := vmCacheDir(t)
	runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

	// Killing the runner and the stopped VM process leaves no process that could clean up.
	for _, pid := range []int{runner.Process.Pid, vmPID} {
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err != nil {
			t.Fatal(err)
		}
	}

	_ = runner.Wait() // The process was killed.

	waitForNoProcess(t, cacheDir)

	if CheckClean(cacheDir) == nil {
		t.Fatal("no run state remains after SIGKILL, so the test checks nothing")
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), runArgs(busybox, "true")...); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	err := CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestVMLiveVMKeepsItsRunState checks that a VM process that outlives its runner keeps its run root and the cache.
func TestVMLiveVMKeepsItsRunState(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]
	cacheDir := vmCacheDir(t)
	runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

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

	if res := sbxr(t, bin, vmEnv(cacheDir), runArgs(busybox, "true")...); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), "prune", "--force"); res.code != 1 || !strings.Contains(res.stderr, "a running sbxr run uses the image cache") {
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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMGoneStdoutReaderStopsTheRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs(vmImages(t)[Busybox], "yes")...)
	cmd.Env = vmEnv(cacheDir)

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

	if code := waitExit(t, cmd); code != 141 || stderr.Len() > 0 {
		t.Errorf("sbxr run yes = %d, stderr %q; want 141 and no sbxr error", code, stderr.String())
	}

	waitForNoProcess(t, cacheDir)

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMGoneStdoutReaderStopsATTYRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	tm := startOnTerminal(t, vmBinary(t), vmEnv(cacheDir), pw, nil, runArgs("-q", "-t", vmImages(t)[Busybox], "yes")...)
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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestVMGoneReaderOfDebugOutputStopsTheRun closes a pipe that carries both stdout and the debug logs on stderr.
// A later log write must not kill the runner before it cleans up.
func TestVMGoneReaderOfDebugOutputStopsTheRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), vmBinary(t), append([]string{"--debug"}, runArgs(vmImages(t)[Busybox], "yes")...)...)
	cmd.Env = vmEnv(cacheDir)
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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestVMReaderGoneAfterCtrlCWithStalledOutput closes a stalled stdout reader after Ctrl-C, while an output copy still blocks.
func TestVMReaderGoneAfterCtrlCWithStalledOutput(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs(vmImages(t)[Busybox], "yes")...)
	cmd.Env = vmEnv(cacheDir)
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

	waitForFullPipe(t, pr)

	err = cmd.Process.Signal(syscall.SIGINT)
	if err != nil {
		t.Fatal(err)
	}

	// The runner stops the VM process when it handles the signal. Only then does the reader go away, or the run ends with 141.
	waitForNoProcess(t, cacheDir)

	_ = pr.Close()

	if code := waitExit(t, cmd); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}

	waitForNoProcess(t, cacheDir)

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMKilledVMProcessIsAnSbxrError(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs(vmImages(t)[Busybox], "sh", "-c", "echo ready; sleep 30")...)
	cmd.Env = vmEnv(cacheDir)

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

	err = syscall.Kill(vmProcess(t, cacheDir), syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	_ = cmd.Wait() // The exit code below shows the outcome.

	want := "sbxr: vm: the VM process was killed by signal 9; run again with --debug and report a bug\n"
	if code := cmd.ProcessState.ExitCode(); code != 125 || stderr.String() != want {
		t.Errorf("sbxr run = %d, stderr %q; want 125 and %q", code, stderr.String(), want)
	}

	waitForNoProcess(t, cacheDir)

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMRunWithTOSTOP(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

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

	for _, args := range [][]string{runArgs(busybox, "echo", "hi"), runArgs("-t", busybox, "echo", "hi")} {
		out, code, _ := startOnTerminal(t, bin, vmEnv(vmCacheDir(t)), nil, tostop, args...).wait()
		if code != 0 || !strings.Contains(out, "hi") {
			t.Errorf("sbxr %q = %d, %q; want 0 and hi", args, code, out)
		}
	}
}

func TestVMSIGTERMWithAStalledTTYReader(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)
	word := strings.Repeat("x", 100000)

	// Nobody reads this pipe, so the output copy stalls once the pipe buffer is full.
	stdout, unread, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs("-t", vmImages(t)[Busybox], "echo", word, word)...)
	cmd.Env = vmEnv(cacheDir)
	cmd.Stdout = unread

	err = cmd.Start()
	_ = unread.Close()

	if err != nil {
		t.Fatal(err)
	}

	// Output in the pipe means that the runner handles signals.
	waitForFullPipe(t, stdout)

	err = cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}

	if code := waitExit(t, cmd); code != 143 {
		t.Errorf("exit code = %d, want 143", code)
	}

	waitForNoProcess(t, cacheDir)

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMTTYWithoutInteractiveNeverReadsStdin(t *testing.T) {
	t.Parallel()

	// The test keeps the pipe open, like a terminal that the user never types into.
	stdin, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keepOpen.Close()
	defer stdin.Close()

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs("-t", vmImages(t)[Busybox], "cat")...)
	cmd.Env = vmEnv(vmCacheDir(t))
	cmd.Stdin = stdin

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	if code := waitExit(t, cmd); code != 0 {
		t.Errorf("sbxr run -t cat = %d, want 0 after it reads an empty stdin", code)
	}
}

func TestVMTTYOutputReachesASlowReader(t *testing.T) {
	t.Parallel()

	// The output is larger than a pipe buffer, so the runner must wait for the reader.
	word := strings.Repeat("x", 50000)
	want := strings.Join([]string{word, word, word, word}, " ") + "\r\n"

	cmd := exec.CommandContext(t.Context(), vmBinary(t), runArgs("-t", vmImages(t)[Busybox], "echo", word, word, word, word)...)
	cmd.Env = vmEnv(vmCacheDir(t))

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

// TestVMVolumeSymlinksCannotEscape mounts a volume through symlinks in an earlier volume that lead to a reserved path,
// the guest root, or through a file.
func TestVMVolumeSymlinksCannotEscape(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)
	cacheDir := vmCacheDir(t)

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
		res := sbxr(t, bin, vmEnv(cacheDir), runArgs("-q", "-v", links+":/a", "-v", target+":/a/"+name, refs[Busybox], "true")...)
		if res.code != 125 || !strings.Contains(res.stderr, want) {
			t.Errorf("volume at /a/%s: sbxr run = %d, stderr %q; want 125 and %q", name, res.code, res.stderr, want)
		}
	}

	if err := CheckClean(cacheDir); err != nil {
		t.Error(err)
	}
}

// runArgs returns the arguments of `sbxr run` with the small VM flags and then args.
func runArgs(args ...string) []string {
	return append(append([]string{"run"}, vmFlags...), args...)
}

// raw reports whether the terminal has line editing off.
func (tm *terminal) raw() bool {
	termios, err := unix.IoctlGetTermios(int(tm.tty.Fd()), getTermios)

	return err == nil && termios.Lflag&unix.ICANON == 0
}

// waitForFullPipe waits until the read end r of a pipe holds unread output that stops growing.
// The output of the workloads that use it is larger than the pipe, so then the writer blocks.
func waitForFullPipe(t *testing.T, r *os.File) {
	t.Helper()

	last, same := 0, 0

	for deadline := time.Now().Add(10 * time.Second); same < 5; time.Sleep(50 * time.Millisecond) {
		n, err := unix.IoctlGetInt(int(r.Fd()), unreadBytes)
		if err != nil {
			t.Fatal(err)
		}

		if time.Now().After(deadline) {
			t.Fatalf("the pipe holds %d bytes that still change", n)
		}

		same++
		if n == 0 || n != last {
			last, same = n, 0
		}
	}
}

// startStoppedVM starts a run, waits until its workload runs, and stops the VM process with SIGSTOP.
// It returns the runner and the VM process ID.
func startStoppedVM(t *testing.T, bin, cacheDir, image string) (*exec.Cmd, int) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), bin, runArgs(image, "sh", "-c", "echo ready; sleep 30")...)
	cmd.Env = vmEnv(cacheDir)

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

	pid := vmProcess(t, cacheDir)

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
