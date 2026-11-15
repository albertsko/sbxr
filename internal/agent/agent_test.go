//go:build linux

package agent

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestTerminalAttachBeforeStart attaches the streams of a terminal exec before crun hands over its console, as the
// shim does, and checks that input, output, and an early resize all reach the terminal.
func TestTerminalAttachBeforeStart(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t)

	p, err := newProcess(true, true, true, false)
	if err != nil {
		t.Fatal(err)
	}

	s := newService(newReaper(), slog.New(slog.DiscardHandler))
	s.containers["c1"] = &container{bundle: "", init: p, started: true, execs: map[string]*process{}}

	stdinHost, stdinAgent := net.Pipe()
	stdoutHost, stdoutAgent := net.Pipe()

	go p.attach("stdin", stdinAgent, stdinAgent)
	go p.attach("stdout", stdoutAgent, stdoutAgent)

	_, err = s.ResizePty(t.Context(), &taskAPI.ResizePtyRequest{ID: "c1", Width: 120, Height: 40})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond) // The streams wait for the console now.
	p.setConsole(master)

	ws, err := unix.IoctlGetWinsize(int(slave.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col != 120 || ws.Row != 40 {
		t.Errorf("terminal size %+v, %v; want 120x40", ws, err)
	}

	go func() { _, _ = stdinHost.Write([]byte("typed\n")) }()

	line, err := bufio.NewReader(slave).ReadString('\n')
	if err != nil || line != "typed\n" {
		t.Errorf("terminal input %q, %v", line, err)
	}

	_, _ = slave.WriteString("shown\n")

	out := make([]byte, 64)
	n, err := io.ReadAtLeast(stdoutHost, out, len("shown"))

	if err != nil || !strings.HasPrefix(string(out[:n]), "shown") {
		t.Errorf("terminal output %q, %v", out[:n], err)
	}
}

// TestTerminalStartFailureEndsStreams checks that streams of a terminal process that never started end.
func TestTerminalStartFailureEndsStreams(t *testing.T) {
	t.Parallel()

	p, err := newProcess(true, false, true, false)
	if err != nil {
		t.Fatal(err)
	}

	host, agent := net.Pipe()
	done := make(chan struct{})

	go func() {
		p.attach("stdout", agent, agent)
		close(done)
	}()

	p.setConsole(nil)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of a process without a console did not end")
	}

	_ = host.Close()
}

// TestExecFilesGo runs many exec probes that fail or end and checks that the crun root keeps no file of them.
func TestExecFilesGo(t *testing.T) {
	t.Parallel()

	s := newService(newReaper(), slog.New(slog.DiscardHandler))
	s.root, s.crun = t.TempDir(), "/nonexistent/crun"
	s.containers["c1"] = &container{bundle: "", init: &process{done: make(chan struct{}), consoleReady: make(chan struct{})}, started: true, execs: map[string]*process{}}

	ctx := context.Background()
	spec := &anypb.Any{Value: []byte(`{"args":["true"],"cwd":"/"}`)}

	for i := range 50 {
		execID := "probe-" + strconv.Itoa(i)

		_, err := s.Exec(ctx, &taskAPI.ExecProcessRequest{ID: "c1", ExecID: execID, Stdout: "out", Spec: spec, Terminal: i%2 == 0})
		if err != nil {
			t.Fatal(err)
		}

		// Half of them fail to start, as with a missing command.
		if i%2 == 0 {
			if _, err := s.Start(ctx, &taskAPI.StartRequest{ID: "c1", ExecID: execID}); err == nil {
				t.Fatal("start with a missing crun succeeded")
			}
		}

		if _, err := s.Delete(ctx, &taskAPI.DeleteRequest{ID: "c1", ExecID: execID}); err != nil {
			t.Fatal(err)
		}
	}

	if entries, _ := os.ReadDir(s.root); len(entries) != 0 {
		t.Errorf("crun root keeps %d files, first %s", len(entries), entries[0].Name())
	}

	if _, err := s.Exec(ctx, &taskAPI.ExecProcessRequest{ID: "c1", ExecID: "../x", Spec: spec}); err == nil {
		t.Error("an exec id with a slash was accepted")
	}
}

// openPTY returns a pseudo-terminal master and its slave.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminals:", err)
	}

	unlock := 0

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if errno != 0 {
		t.Fatal(errno)
	}

	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}

	// No echo, so the input test reads only what the master wrote.
	termios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err == nil {
		termios.Lflag &^= unix.ECHO
		_ = unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, termios)
	}

	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})

	return master, slave
}
