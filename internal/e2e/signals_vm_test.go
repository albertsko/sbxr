//go:build vm

package e2e

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
