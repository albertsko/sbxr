//go:build vm

package e2e

import (
	"bytes"
	"errors"
	"io"
	"os"
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

			cmd := vmCommand(t, bin, vmEnv(cacheDir), runArgs(busybox, "sh", "-c", "echo ready; sleep 30")...)

			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}

			err = cmd.Start()
			if err != nil {
				t.Fatal(err)
			}

			readReady(t, cmd, stdout, readText(stdout, "ready\n"))

			runner := runnerOf(t, cmd, vmProcess(t, cacheDir))

			err = cmd.Process.Signal(tt.signal)
			if err != nil {
				t.Fatal(err)
			}

			_ = cmd.Wait() // The exit code below shows the outcome.

			if code := cmd.ProcessState.ExitCode(); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}

			// After a SIGKILL, the VM process removes the run state when it sees the parent is gone.
			// On Linux, the signal kills only the user namespace wrapper, and the runner gets SIGTERM and cleans up after the wrapper exits.
			waitForNoProcess(t, cacheDir)
			waitForGone(t, runner)

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

func TestVMCtrlCReachesAWorkloadThatReadsIt(t *testing.T) {
	t.Parallel()

	// Without ISIG, the guest terminal passes Ctrl-C to the workload as a byte, as a shell or an editor wants it.
	script := "stty raw -echo; echo raw; head -c 1 | od -An -tx1"
	tm := startOnTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), nil, nil, runArgs("-it", vmImages(t)[Busybox], "sh", "-c", script)...)
	tm.waitForOutput("raw")
	tm.write("\x03")

	out, code, _ := tm.wait()
	if code != 0 || !strings.Contains(out, " 03") {
		t.Errorf("sbxr run -it then Ctrl-C to a raw terminal = %d, %q; want 0 and the byte 03", code, out)
	}
}

func TestVMCtrlCReachesAWorkloadThatKeepsWriting(t *testing.T) {
	t.Parallel()

	// The terminal of the test drains the output at once, so the output never stalls and the guest still reads input.
	script := "stty raw -echo; echo raw; yes & head -c 1 | od -An -tx1; kill $!"
	tm := startOnTerminal(t, vmBinary(t), vmEnv(vmCacheDir(t)), nil, nil, runArgs("-it", vmImages(t)[Busybox], "sh", "-c", script)...)
	tm.waitForOutput("raw")
	tm.waitForOutput("y\ny\n")
	tm.write("\x03")

	out, code, _ := tm.wait()
	if code != 0 || !strings.Contains(out, " 03") {
		t.Errorf("sbxr run -it then Ctrl-C to a raw terminal with steady output = %d, %d bytes; want 0 and the byte 03", code, len(out))
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

	full := fillPipe(t, stdout, unread)

	tm := startOnTerminal(t, vmBinary(t), vmEnv(cacheDir), unread, nil, runArgs("-it", vmImages(t)[Busybox], "echo", word, word)...)

	for deadline := time.Now().Add(10 * time.Second); !tm.raw(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the terminal never became raw")
		}
	}

	// The workload blocks on the full output, so the guest reads no input. The stalled drain would keep the runner waiting after the VM exits.
	waitForStalledWriter(t, stdout, unread, full)
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

	if tm.raw() {
		t.Error("the terminal stayed raw after the run")
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
	defer pw.Close()

	full := fillPipe(t, pr, pw)

	cmd := vmCommand(t, vmBinary(t), vmEnv(cacheDir), runArgs(vmImages(t)[Busybox], "yes")...)
	cmd.Stdout = pw

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Output in the pipe proves that the runner handles signals. Nobody reads, so the output copy then blocks on the full pipe.
	waitForStalledWriter(t, pr, pw, full)

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

	cmd := vmCommand(t, vmBinary(t), vmEnv(cacheDir), runArgs(vmImages(t)[Busybox], "sh", "-c", "echo ready; sleep 30")...)

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

	readReady(t, cmd, stdout, readText(stdout, "ready\n"))

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
	defer unread.Close()

	full := fillPipe(t, stdout, unread)

	cmd := vmCommand(t, vmBinary(t), vmEnv(cacheDir), runArgs("-t", vmImages(t)[Busybox], "echo", word, word)...)
	cmd.Stdout = unread

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Output in the pipe means that the runner handles signals.
	waitForStalledWriter(t, stdout, unread, full)

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

// fillPipe fills the pipe with the ends r and w before a run writes to w, so that the first write of the run stalls.
// It returns the number of bytes in the full pipe.
func fillPipe(t *testing.T, r, w *os.File) int {
	t.Helper()

	raw, err := w.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 4096)

	var writeErr error

	err = raw.Control(func(fd uintptr) {
		// The pipe is non-blocking, so EAGAIN means that it is full.
		for writeErr == nil || errors.Is(writeErr, unix.EINTR) {
			_, writeErr = unix.Write(int(fd), buf)
		}
	})
	if !errors.Is(writeErr, unix.EAGAIN) {
		err = errors.Join(err, writeErr)
	}

	if err != nil {
		t.Fatal(err)
	}

	return unreadInPipe(t, r)
}

// waitForStalledWriter reads one page from the pipe that fillPipe filled and waits until a writer has refilled it.
// Linux frees pipe space by pages. The pipe then has no room for a write of PIPE_BUF bytes,
// and the workloads that use it write far more than that, so the writer blocks.
func waitForStalledWriter(t *testing.T, r, w *os.File, full int) {
	t.Helper()

	page := os.Getpagesize()

	_, err := io.ReadFull(r, make([]byte, page))
	if err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(readyTimeout); ; time.Sleep(10 * time.Millisecond) {
		n := unreadInPipe(t, r)
		if n > full-page && !writable(t, w) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("the pipe holds %d of %d bytes and is writable: %v; want a writer that refilled it", n, full, writable(t, w))
		}
	}
}

// unreadInPipe returns the number of unread bytes in the pipe with the read end r.
func unreadInPipe(t *testing.T, r *os.File) int {
	t.Helper()

	raw, err := r.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	var (
		n       int
		ioctErr error
	)

	err = raw.Control(func(fd uintptr) { n, ioctErr = unix.IoctlGetInt(int(fd), unreadBytes) })
	if err = errors.Join(err, ioctErr); err != nil {
		t.Fatal(err)
	}

	return n
}

// writable reports whether the pipe with the write end w has room for a write of PIPE_BUF bytes.
func writable(t *testing.T, w *os.File) bool {
	t.Helper()

	raw, err := w.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	fds := []unix.PollFd{{Fd: 0, Events: unix.POLLOUT, Revents: 0}}

	var pollErr error

	err = raw.Control(func(fd uintptr) {
		fds[0].Fd = int32(fd)

		for {
			_, pollErr = unix.Poll(fds, 0)
			if !errors.Is(pollErr, unix.EINTR) {
				return
			}
		}
	})
	if err = errors.Join(err, pollErr); err != nil {
		t.Fatal(err)
	}

	return fds[0].Revents&unix.POLLOUT != 0
}
