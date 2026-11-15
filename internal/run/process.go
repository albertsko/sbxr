package run

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// withSignals returns a context that a SIGINT, SIGTERM, or SIGHUP (closed terminal) cancels with a signalError cause.
func withSignals(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

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
//
// The VM process stays in the runner's process group, so job control and TOSTOP treat both alike.
// It ignores terminal interrupts (see vm.Child), so Ctrl-C ends the run only through the runner.
//
// The VM process inherits the locks, so that sweeps and prune leave the run alone while the VM lives, even after the runner died.
func (r *Runner) start(ctx context.Context, s spec.Spec, dir string, locks ...*os.File) (int, error) {
	cmd := exec.CommandContext(ctx, r.Exe, vm.ChildArg, dir) //nolint:gosec // Exe is the sbxr binary itself.
	cmd.Env = append(os.Environ(), r.VMEnv...)

	cmd.ExtraFiles = locks

	release, err := vm.AttachLifeline(cmd)
	if err != nil {
		return ExitSetup, fmt.Errorf("start vm process: %w", err)
	}
	defer release()

	if s.TTY {
		err = r.runTTY(ctx, cmd, s.Interactive)
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
		// The VM process ignores terminal signals, so a signal death means that something else killed it or the VMM crashed.
		status, _ := exit.Sys().(syscall.WaitStatus)

		return ExitSetup, &spec.Error{
			Source:  "vm",
			Problem: fmt.Sprintf("the VM process was killed by signal %d", int(status.Signal())),
			Fix:     "run again with --debug and report a bug",
		}
	default:
		return ExitSetup, &spec.Error{Source: "vm", Problem: err.Error()}
	}
}

// runPlain gives the VM process the user's stdout and stderr, and stdin only when interactive.
func (r *Runner) runPlain(cmd *exec.Cmd, interactive bool) error {
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr

	if interactive {
		cmd.Stdin = r.Stdin
	}

	return cmd.Run() //nolint:wrapcheck // start inspects the exec error.
}

// runTTY gives the VM process a new pseudo-terminal for its output, and for its input only when interactive.
// Without -i, the VM process reads an empty stdin, so it never reads the user's terminal.
func (r *Runner) runTTY(ctx context.Context, cmd *exec.Cmd, interactive bool) error {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("open pseudo-terminal: %w", err)
	}
	defer ptmx.Close() //nolint:errcheck // The output copy has ended here.

	// The runner keeps tty open until the output is copied, because macOS drops unread output at the last close.
	defer tty.Close() //nolint:errcheck // See above.

	// The guest kernel is the only terminal: it echoes, edits lines, and turns keys such as Ctrl-C into signals.
	// The runner sets the terminal raw before any input arrives, so that no key is lost.
	_, err = term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return fmt.Errorf("set pseudo-terminal raw mode: %w", err)
	}

	// The terminal is not the VM process's controlling terminal, because the exit of a controlling process
	// hangs the terminal up, and then macOS drops unread output and the end marker cannot be written.
	cmd.Stdout, cmd.Stderr = tty, tty

	if interactive {
		cmd.Stdin = tty
	}

	userTerm := term.IsTerminal(int(r.Stdin.Fd()))
	if userTerm {
		_ = pty.InheritSize(r.Stdin, ptmx) // A failed resize keeps the default size.
	}

	// validate ensures that the user's stdin is a terminal with -i -t.
	restore := func() {}

	if interactive {
		state, err := term.MakeRaw(int(r.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}

		restore = sync.OnceFunc(func() {
			_ = term.Restore(int(r.Stdin.Fd()), state) // Nothing is left to do when the restore fails.
		})
		defer restore()
	}

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start vm process: %w", err)
	}

	if userTerm {
		stopResize := forwardResize(r.Stdin, ptmx, cmd.Process)
		defer stopResize()
	}

	if interactive {
		go func() {
			_, _ = io.Copy(ptmx, r.Stdin) // The VM exit ends the run, not the input copy.
		}()
	}

	marker := rand.Text()
	out := &guardedWriter{w: r.Stdout, closed: atomic.Bool{}}
	copied := make(chan struct{})

	defer out.close()

	go func() {
		defer close(copied)

		copyUntil(out, ptmx, []byte(marker))
	}()

	err = cmd.Wait()

	// The user's terminal leaves raw mode before the drain, so that Ctrl-C during a stalled drain is a SIGINT that ends it.
	restore()

	// The marker follows all VM output through the terminal, so the copy ends exactly after that output.
	// The write can block while a slow reader holds the output back, so it runs on its own.
	go func() {
		_, _ = io.WriteString(tty, marker) // A failed write leaves the wait below to the context.
	}()

	select {
	case <-copied:
	case <-ctx.Done():
	}

	return err //nolint:wrapcheck // start inspects the exec error.
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

// guardedWriter drops writes that start after close, so that no output starts after the run ends.
// It uses no lock, because a write can block on a slow reader.
type guardedWriter struct {
	w      io.Writer
	closed atomic.Bool
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	if g.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	return g.w.Write(p) //nolint:wrapcheck // This is a plain write.
}

func (g *guardedWriter) close() { g.closed.Store(true) }

// forwardResize copies the terminal size of from to the pseudo-terminal on every SIGWINCH and tells the VM process.
// The VM process must be told, because the pseudo-terminal is not its controlling terminal.
func forwardResize(from, ptmx *os.File, vmProcess *os.Process) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	go func() {
		for range winch {
			_ = pty.InheritSize(from, ptmx)        // A failed resize keeps the old size.
			_ = vmProcess.Signal(syscall.SIGWINCH) // The VM process may have exited already.
		}
	}()

	return func() {
		signal.Stop(winch)
		close(winch)
	}
}
