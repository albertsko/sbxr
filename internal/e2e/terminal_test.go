package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// terminal is a run of sbxr on a pseudo-terminal of 30 rows and 100 columns that never answers terminal queries.
type terminal struct {
	t     *testing.T
	ptmx  *os.File
	tty   *os.File
	cmd   *exec.Cmd
	start time.Time
	out   syncBuffer
}

// onTerminal runs bin on a new terminal and returns the terminal output, the exit code, and the run time.
func onTerminal(t *testing.T, bin string, environ []string, args ...string) (string, int, time.Duration) {
	t.Helper()

	return startOnTerminal(t, bin, environ, nil, nil, args...).wait()
}

// startOnTerminal starts bin with stdin, stdout, and stderr on a new terminal, for at most 20 seconds.
// A non-nil stdout replaces the terminal as stdout. A non-nil setup changes the terminal before the start.
func startOnTerminal(t *testing.T, bin string, environ []string, stdout io.Writer, setup func(tty *os.File), args ...string) *terminal {
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

	go func() { _, _ = io.Copy(&tm.out, ptmx) }() // The copy ends when the terminal closes.

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	return tm
}

func (tm *terminal) output() string { return tm.out.String() }

// waitForOutput waits until the terminal output contains text.
func (tm *terminal) waitForOutput(text string) {
	tm.t.Helper()

	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(tm.output(), text); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = tm.cmd.Process.Kill()
			_ = tm.cmd.Wait() // The kill decides the outcome.

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
