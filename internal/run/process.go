package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// After the VM process exits, the runner copies terminal output until it is quiet for drainIdle, at most drainTimeout.
const (
	drainIdle    = 50 * time.Millisecond
	drainTimeout = time.Second
)

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

	var exit *exec.ExitError

	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.Exited():
		return exit.ExitCode(), nil
	case ctx.Err() != nil:
		return ExitSetup, fmt.Errorf("vm process: %w", context.Cause(ctx))
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

// runTTY gives the VM process a new pseudo-terminal as its controlling terminal.
func (r *Runner) runTTY(cmd *exec.Cmd, interactive bool) error {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return fmt.Errorf("open pseudo-terminal: %w", err)
	}
	defer ptmx.Close() //nolint:errcheck // Closing the terminal ends the output copy.

	// macOS drops unread output when the last terminal file closes, so the runner keeps tty open until the output is read.
	defer tty.Close() //nolint:errcheck // See above.

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty = true, true

	userTerm := term.IsTerminal(int(r.Stdin.Fd()))
	if userTerm {
		_ = pty.InheritSize(r.Stdin, ptmx) // A failed resize keeps the default size.

		stopResize := forwardResize(r.Stdin, ptmx)
		defer stopResize()
	}

	if userTerm && interactive {
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

	output := make(chan struct{}, 1)

	go func() {
		buf := make([]byte, 32*1024)

		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				_, _ = r.Stdout.Write(buf[:n]) // A closed stdout does not stop the VM.

				select {
				case output <- struct{}{}:
				default:
				}
			}

			if err != nil {
				return
			}
		}
	}()

	err = cmd.Wait()

	drain(output)

	return err //nolint:wrapcheck // start inspects the exec error.
}

// drain waits until output has been quiet for a moment, at most drainTimeout.
func drain(output <-chan struct{}) {
	deadline := time.After(drainTimeout)

	for {
		select {
		case <-output:
		case <-time.After(drainIdle):
			return
		case <-deadline:
			return
		}
	}
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
