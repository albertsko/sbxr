package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// drainTimeout bounds the terminal output copy when the end marker could not be written.
const drainTimeout = time.Second

// withSignals returns a context that a SIGINT or SIGTERM cancels with a signalError cause.
func withSignals(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			s, _ := sig.(syscall.Signal) // Notify delivers only syscall signals.
			cancel(signalError{sig: s})
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(signals)
		cancel(nil)
	}
}

// start runs the VM process and returns the workload exit code.
// The context kills the VM process at once.
func (r *Runner) start(ctx context.Context, s spec.Spec, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, r.Exe, vm.ChildArg, dir) //nolint:gosec // Exe is the sbxr binary itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{}

	release, err := vm.AttachLifeline(cmd)
	if err != nil {
		return ExitSetup, fmt.Errorf("start vm process: %w", err)
	}
	defer release()

	if s.TTY {
		err = r.runTTY(cmd, s.Interactive)
	} else {
		err = r.runPlain(cmd, s.Interactive)
	}

	exit, isExit := errors.AsType[*exec.ExitError](err)

	switch {
	case err == nil:
		return 0, nil
	case ctx.Err() != nil:
		return ExitSetup, fmt.Errorf("vm process: %w", context.Cause(ctx))
	case isExit && exit.Exited():
		return exit.ExitCode(), nil
	case isExit:
		// A signal that the runner did not send, such as Ctrl-C through the terminal, ends the VM like a workload.
		status, _ := exit.Sys().(syscall.WaitStatus)

		return 128 + int(status.Signal()), nil
	default:
		return ExitSetup, &spec.Error{Source: "vm", Problem: err.Error()}
	}
}

// runPlain gives the VM process the user's stdout and stderr, and stdin only when interactive.
// The VM process gets its own process group, so terminal signals reach only the runner.
func (r *Runner) runPlain(cmd *exec.Cmd, interactive bool) error {
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	cmd.SysProcAttr.Setpgid = true

	if interactive {
		cmd.Stdin = r.Stdin
	}

	// A background process group that reads the terminal is stopped, so the runner copies terminal input.
	if interactive && term.IsTerminal(int(r.Stdin.Fd())) {
		in, err := r.copyStdin()
		if err != nil {
			return err
		}

		defer in.Close() //nolint:errcheck // The VM process has its own copy.

		cmd.Stdin = in
	}

	return cmd.Run() //nolint:wrapcheck // start inspects the exec error.
}

// copyStdin returns a pipe that receives the user's stdin.
// The copy ends when the user's stdin closes. A VM exit does not stop it, which is harmless for one run.
func (r *Runner) copyStdin() (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	go func() {
		_, _ = io.Copy(pw, r.Stdin) // Only the end of the copy matters.
		_ = pw.Close()              // The VM sees EOF.
	}()

	return pr, nil
}

// runTTY gives the VM process a new pseudo-terminal for its output, and for its input only when interactive.
// Without -i, the VM process reads an empty stdin, so it never reads the user's terminal.
func (r *Runner) runTTY(cmd *exec.Cmd, interactive bool) error {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("open pseudo-terminal: %w", err)
	}
	defer ptmx.Close() //nolint:errcheck // The output copy has ended here.

	// The runner keeps tty open until the output is copied, because macOS drops unread output at the last close.
	defer tty.Close() //nolint:errcheck // See above.

	// The terminal is not the VM process's controlling terminal, because the exit of a controlling process
	// hangs the terminal up, and then macOS drops unread output and the end marker cannot be written.
	cmd.Stdout, cmd.Stderr = tty, tty
	cmd.SysProcAttr.Setpgid = true

	if interactive {
		cmd.Stdin = tty
	}

	if term.IsTerminal(int(r.Stdin.Fd())) {
		_ = pty.InheritSize(r.Stdin, ptmx) // A failed resize keeps the default size.

		stopResize := forwardResize(r.Stdin, ptmx)
		defer stopResize()
	}

	// validate ensures that the user's stdin is a terminal with -i -t.
	if interactive {
		state, err := term.MakeRaw(int(r.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}

		defer term.Restore(int(r.Stdin.Fd()), state) //nolint:errcheck // Nothing is left to do when the restore fails.
	}

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start vm process: %w", err)
	}

	if interactive {
		go func() {
			_, _ = io.Copy(ptmx, r.Stdin) // The VM exit ends the run, not the input copy.
		}()
	}

	marker := rand.Text()
	out := &guardedWriter{mu: sync.Mutex{}, w: r.Stdout, closed: false}
	copied := make(chan struct{})

	go func() {
		defer close(copied)

		copyUntil(out, ptmx, []byte(marker))
	}()

	err = cmd.Wait()

	// The marker follows all VM output through the terminal, so the copy ends exactly after that output.
	_, markErr := io.WriteString(tty, marker)
	if markErr != nil {
		markErr = fmt.Errorf("end terminal output: %w", markErr)
	}

	select {
	case <-copied:
	case <-time.After(markerTimeout(markErr)):
	}

	out.close()

	return errors.Join(err, markErr)
}

// markerTimeout waits for the copy without limit, unless the marker could not be written.
func markerTimeout(markErr error) time.Duration {
	if markErr != nil {
		return drainTimeout
	}

	return math.MaxInt64
}

// copyUntil copies src to dst until it reads marker, which it drops.
// It also ends at a read error. Write errors do not stop it, so that the VM never blocks on output.
func copyUntil(dst io.Writer, src io.Reader, marker []byte) {
	buf := make([]byte, 32*1024)

	var pending []byte

	for {
		n, err := src.Read(buf)
		pending = append(pending, buf[:n]...)

		if i := bytes.Index(pending, marker); i >= 0 {
			_, _ = dst.Write(pending[:i])

			return
		}

		keep := 0
		if err == nil {
			keep = markerStart(pending, marker)
		}

		_, _ = dst.Write(pending[:len(pending)-keep])
		pending = append(pending[:0], pending[len(pending)-keep:]...)

		if err != nil {
			return
		}
	}
}

// markerStart returns the length of the longest end of b that starts marker.
// Only those bytes wait for the next read, so that output never stalls.
func markerStart(b, marker []byte) int {
	for n := min(len(b), len(marker)-1); n > 0; n-- {
		if bytes.HasSuffix(b, marker[:n]) {
			return n
		}
	}

	return 0
}

// guardedWriter drops writes after close, so that no output appears after the run ends.
type guardedWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		return 0, io.ErrClosedPipe
	}

	return g.w.Write(p) //nolint:wrapcheck // This is a plain write.
}

func (g *guardedWriter) close() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.closed = true
}

// forwardResize copies the terminal size of from to the pseudo-terminal on every SIGWINCH.
func forwardResize(from, ptmx *os.File) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	go func() {
		for range winch {
			_ = pty.InheritSize(from, ptmx) // A failed resize keeps the old size.
		}
	}()

	return func() {
		signal.Stop(winch)
		close(winch)
	}
}
