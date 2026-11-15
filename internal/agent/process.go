//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// crun runs containers in the guest. Its state lives in guest memory.
const (
	crunPath = "/usr/bin/crun"
	crunRoot = "/run/crun"
)

// process is a container's init process or an exec process, with the host's end of its stdio.
type process struct {
	terminal bool
	// stdin, stdout, and stderr are the agent's pipe ends. Each is nil when the host did not ask for the stream.
	stdin, stdout, stderr *os.File
	// console is the pseudo-terminal master of a terminal process.
	console *os.File
	// child holds the process's own stdio ends until crun has started it.
	child [3]*os.File

	mu   sync.Mutex
	pid  int
	done chan struct{}
	exit exit
}

// newProcess creates the pipes for the streams that the host asked for. A terminal process gets them from its console instead.
func newProcess(terminal, stdin, stdout, stderr bool) (*process, error) {
	p := &process{}
	p.terminal = terminal
	p.done = make(chan struct{})

	if terminal {
		return p, nil
	}

	want := []bool{stdin, stdout, stderr}

	for i, ok := range want {
		if !ok {
			continue
		}

		r, w, err := os.Pipe()
		if err != nil {
			p.closeAll()

			return nil, fmt.Errorf("create stdio pipe: %w", err)
		}

		if i == 0 {
			p.child[0], p.stdin = r, w
		} else {
			p.child[i] = w

			if i == 1 {
				p.stdout = r
			} else {
				p.stderr = r
			}
		}
	}

	return p, nil
}

// stdio returns the process's own stdio for crun, with /dev/null for missing streams, and a function that closes the /dev/null files.
func (p *process) stdio() ([3]*os.File, func(), error) {
	var (
		files [3]*os.File
		nulls []*os.File
	)

	closeNulls := func() {
		for _, f := range nulls {
			_ = f.Close()
		}
	}

	for i, f := range p.child {
		if f != nil {
			files[i] = f

			continue
		}

		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			closeNulls()

			return files, nil, fmt.Errorf("open %s: %w", os.DevNull, err)
		}

		files[i] = null
		nulls = append(nulls, null)
	}

	return files, closeNulls, nil
}

// started closes the agent's copies of the process's stdio ends, so the pipes end when the process does.
func (p *process) started() {
	for i, f := range p.child {
		if f != nil {
			_ = f.Close()
			p.child[i] = nil
		}
	}
}

// watch records the process's exit when it comes.
func (p *process) watch(pid int, exited <-chan exit, log *slog.Logger) {
	p.mu.Lock()
	p.pid = pid
	p.mu.Unlock()

	go func() {
		e := <-exited
		log.Info("exited", "pid", pid, "status", e.Status)

		p.mu.Lock()
		p.exit = e
		p.mu.Unlock()

		close(p.done)
	}()
}

func (p *process) pidAndExit() (int, exit, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return p.pid, p.exit, true
	default:
		return p.pid, p.exit, false
	}
}

func (p *process) closeAll() {
	for _, f := range append(p.child[:], p.stdin, p.stdout, p.stderr, p.console) {
		if f != nil {
			_ = f.Close()
		}
	}
}

// attach connects one stream from the host to the process.
func (p *process) attach(kind string, conn net.Conn, in io.Reader) {
	defer conn.Close() //nolint:errcheck // The stream ends either way.

	switch {
	case kind == "stdin" && p.terminal:
		_, _ = io.Copy(p.console, in) // The console stays open for the output.
	case kind == "stdin" && p.stdin != nil:
		_, _ = io.Copy(p.stdin, in)
		_ = p.stdin.Close() // EOF reaches the process.
	case kind == "stdout" && p.terminal:
		_, _ = io.Copy(conn, p.console) // A closed terminal reads as EIO.
	case kind == "stdout" && p.stdout != nil:
		_, _ = io.Copy(conn, p.stdout)
	case kind == "stderr" && p.stderr != nil:
		_, _ = io.Copy(conn, p.stderr)
	}
}

// crunCmd runs one crun command with stdio and returns crun's own error message when it fails.
func (s *service) crunCmd(stdio [3]*os.File, args ...string) error {
	log, err := os.CreateTemp(crunRoot, "log-*")
	if err != nil {
		return fmt.Errorf("create crun log: %w", err)
	}

	defer os.Remove(log.Name()) //nolint:errcheck // Temporary file.
	defer log.Close()           //nolint:errcheck // Read-only use below.

	full := append([]string{"--root", crunRoot, "--cgroup-manager", "cgroupfs", "--log", log.Name()}, args...)
	cmd := newCmd(crunPath, full, stdio)

	exited, err := s.reaper.start(cmd)
	if err != nil {
		return fmt.Errorf("start crun %s: %w", args[0], err)
	}

	e := <-exited
	_ = cmd.Process.Release()

	if e.Status == 0 {
		return nil
	}

	data, _ := io.ReadAll(log)

	return errors.New(crunMessage(string(data), e.Status))
}

// crunMessage returns the last message of a crun log, such as "executable file `nope` not found in $PATH: No such file or directory".
func crunMessage(log string, status uint32) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Text logs look like "2026-11-15T10:00:00Z: error: message".
		if _, msg, ok := strings.Cut(line, "error: "); ok {
			return msg
		}

		return line
	}

	return "crun exited with status " + strconv.FormatUint(uint64(status), 10)
}

// receiveConsole accepts crun's connection on the console socket and returns the pseudo-terminal master that crun sends.
func receiveConsole(l *net.UnixListener) (*os.File, error) {
	conn, err := l.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("accept console socket: %w", err)
	}
	defer conn.Close() //nolint:errcheck // crun closes its side too.

	buf := make([]byte, 64)
	oob := make([]byte, unix.CmsgSpace(4))

	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("read console fd: %w", err)
	}

	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return nil, fmt.Errorf("parse console fd: %w", errors.Join(err, errors.New("no control message")))
	}

	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return nil, fmt.Errorf("parse console fd: %w", errors.Join(err, errors.New("no file descriptor")))
	}

	return os.NewFile(uintptr(fds[0]), "console"), nil
}

// readPid reads a pid file that crun wrote.
func readPid(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("read pid file %s: %w", filepath.Base(path), err)
	}

	return pid, nil
}
