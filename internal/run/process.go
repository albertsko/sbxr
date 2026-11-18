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

	"github.com/albertsko/sbxr/internal/runspec"
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
// The VM process inherits the cache hold and the run lock, so that sweeps and prune leave the run alone while the VM lives,
// even after the runner died. Its fds are 3 for the lifeline (see vm.AttachLifeline), then extra in order:
// 4 for the hold, 5 for the lock, and 6 (vm.NetFD) for the network socket.
func (r *Runner) start(ctx context.Context, s runspec.Spec, dir string, extra []*os.File) (int, error) {
	cmd := exec.CommandContext(ctx, r.Exe, vm.ChildArg, dir) //nolint:gosec // Exe is the sbxr binary itself.
	cmd.Env = append(os.Environ(), r.VMEnv...)

	cmd.ExtraFiles = extra

	release, err := vm.AttachLifeline(cmd)
	if err != nil {
		return ExitSetup, fmt.Errorf("start vm process: %w", err)
	}
	defer release()

	if s.TTY {
		err = r.runTTY(ctx, cmd, s.Interactive)
	} else {
		err = r.runPlain(ctx, cmd, s.Interactive)
	}

	exit, isExit := errors.AsType[*exec.ExitError](err)

	switch {
	case err == nil:
		return 0, nil
	case errors.Is(err, errBrokenPipe):
		return 128 + int(syscall.SIGPIPE), nil
	case ctx.Err() != nil:
		return ExitSetup, fmt.Errorf("vm process: %w", context.Cause(ctx))
	case isExit && exit.Exited():
		return exit.ExitCode(), nil
	case isExit:
		// The VM process ignores terminal signals, so a signal death means that something else killed it or the VMM crashed.
		status, _ := exit.Sys().(syscall.WaitStatus)

		return ExitSetup, &runspec.Error{
			Source:  "vm",
			Problem: fmt.Sprintf("the VM process was killed by signal %d", int(status.Signal())),
			Fix:     "run again with --debug and report a bug",
		}
	default:
		return ExitSetup, &runspec.Error{Source: "vm", Problem: err.Error()}
	}
}

// runPlain relays the user's stdout and stderr, and stdin only when interactive, through pipes.
// The VM process never sees a terminal without -t, because libkrun sends output to a terminal through its console,
// which mixes the streams and loses stderr when only stdout is redirected.
// The relay drains the output after the VM exits, and the context ends a drain that a slow reader holds back.
func (r *Runner) runPlain(ctx context.Context, cmd *exec.Cmd, interactive bool) error {
	stdout := &guardedWriter{w: r.Stdout, closed: atomic.Bool{}}
	stderr := &guardedWriter{w: r.Stderr, closed: atomic.Bool{}}

	defer stdout.close()
	defer stderr.close()

	// Stderr write errors are ignored.
	pipe := newPipeBreak()

	outPipe, outDone, err := relay(stdout, pipe.onError)
	if err != nil {
		return err
	}

	errPipe, errDone, err := relay(stderr, func(error) {})
	if err != nil {
		_ = outPipe.Close() // The relay ends at once.

		return err
	}

	cmd.Stdout, cmd.Stderr = outPipe, errPipe

	var stdin *os.File

	if interactive {
		pr, pw, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("create stdin pipe: %w", err)
		}
		defer pr.Close() //nolint:errcheck // The VM process has its own copy.

		cmd.Stdin, stdin = pr, pw
	}

	err = cmd.Start()

	// The runner keeps only the read ends, so that each relay ends when the VM process exits.
	_ = outPipe.Close()
	_ = errPipe.Close()

	if err != nil {
		return fmt.Errorf("start vm process: %w", err)
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, r.Stdin) // The VM exit ends the run, not the input copy.
			_ = stdin.Close()              // The workload sees the end of input.
		}()
	}

	stopWatch := pipe.watch(cmd.Process)
	defer stopWatch()

	err = cmd.Wait()

	for _, done := range []<-chan struct{}{outDone, errDone} {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	return pipe.result(err)
}

// errBrokenPipe means that the reader of the user's stdout went away, so the runner stopped the VM.
var errBrokenPipe = errors.New("stdout reader is gone")

// pipeBreak stops the VM process when a write to the user's stdout fails with EPIPE, like SIGPIPE stops a Unix pipeline.
type pipeBreak struct {
	broken chan struct{}
	once   sync.Once
}

func newPipeBreak() *pipeBreak {
	return &pipeBreak{broken: make(chan struct{}), once: sync.Once{}}
}

// onError takes the error of a write to the user's stdout.
func (p *pipeBreak) onError(err error) {
	if errors.Is(err, syscall.EPIPE) {
		p.once.Do(func() { close(p.broken) })
	}
}

// watch kills proc when the pipe breaks, until stop is called.
func (p *pipeBreak) watch(proc *os.Process) (stop func()) {
	exited := make(chan struct{})

	go func() {
		select {
		case <-p.broken:
			_ = proc.Kill() // The VM process may have exited already.
		case <-exited:
		}
	}()

	return func() { close(exited) }
}

// result returns errBrokenPipe when the pipe broke, else err.
func (p *pipeBreak) result(err error) error {
	select {
	case <-p.broken:
		return errBrokenPipe
	default:
		return err
	}
}

// errorWriter passes the errors of its writes to onError.
type errorWriter struct {
	w       io.Writer
	onError func(error)
}

func (e errorWriter) Write(b []byte) (int, error) {
	n, err := e.w.Write(b)
	if err != nil {
		e.onError(err)
	}

	return n, err //nolint:wrapcheck // This is a plain write.
}

// relay returns the write end of a new pipe and copies everything from its read end to w.
// Write errors go to onError and do not stop the copy, so that the VM never blocks on output. done closes at the end of the pipe.
func relay(w io.Writer, onError func(error)) (*os.File, <-chan struct{}, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create output pipe: %w", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)
		defer pr.Close() //nolint:errcheck // The pipe was only read.

		buf := make([]byte, 32*1024)

		for {
			n, err := pr.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					onError(werr)
				}
			}

			if err != nil {
				return
			}
		}
	}()

	return pw, done, nil
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
	pipe := newPipeBreak()
	copied := make(chan struct{})

	defer out.close()

	go func() {
		defer close(copied)

		copyUntil(errorWriter{w: out, onError: pipe.onError}, ptmx, []byte(marker))
	}()

	stopWatch := pipe.watch(cmd.Process)
	defer stopWatch()

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

	return pipe.result(err)
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
