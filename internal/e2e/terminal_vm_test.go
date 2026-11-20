//go:build vm

package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

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
	cmd := vmCommand(t, bin, vmEnv(vmCacheDir(t)), runArgs("-q", refs[Busybox], "sh", "-c", "echo OUT; echo ERR >&2; cat /missing")...)

	var stdout bytes.Buffer

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

func TestVMTTYWithoutInteractiveNeverReadsStdin(t *testing.T) {
	t.Parallel()

	// The test keeps the pipe open, like a terminal that the user never types into.
	stdin, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keepOpen.Close()
	defer stdin.Close()

	cmd := vmCommand(t, vmBinary(t), vmEnv(vmCacheDir(t)), runArgs("-t", vmImages(t)[Busybox], "cat")...)
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

	cmd := vmCommand(t, vmBinary(t), vmEnv(vmCacheDir(t)), runArgs("-t", vmImages(t)[Busybox], "echo", word, word, word, word)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	pipe, ok := stdout.(*os.File)
	if !ok {
		t.Fatalf("%T has no read deadline", stdout)
	}

	err = pipe.SetReadDeadline(time.Now().Add(readyTimeout))
	if err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer

	buf := make([]byte, 4096)

	for err == nil {
		time.Sleep(2 * time.Millisecond)

		var n int

		n, err = stdout.Read(buf)
		got.Write(buf[:n])
	}

	if errors.Is(err, os.ErrDeadlineExceeded) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // The kill decides the outcome.

		t.Fatalf("the output did not end within %v, read %d bytes", readyTimeout, got.Len())
	}

	err = cmd.Wait()
	if err != nil || got.String() != want {
		t.Errorf("sbxr run -t echo = %v with %d bytes, want exit 0 with %d bytes", err, got.Len(), len(want))
	}
}

// raw reports whether the terminal has line editing off.
func (tm *terminal) raw() bool {
	termios, err := unix.IoctlGetTermios(int(tm.tty.Fd()), getTermios)

	return err == nil && termios.Lflag&unix.ICANON == 0
}
