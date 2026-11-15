//go:build linux

package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/albertsko/sbxr/internal/podvm"
)

// service implements the containerd task API for the containers of this pod VM.
type service struct {
	reaper *reaper
	log    *slog.Logger
	// crun is the crun binary, and root its state directory, which also holds the agent's files of each process.
	crun, root string

	mu         sync.Mutex
	containers map[string]*container
}

type container struct {
	bundle  string
	init    *process
	started bool
	execs   map[string]*process
}

func newService(r *reaper, log *slog.Logger) *service {
	return &service{reaper: r, log: log, crun: crunPath, root: crunRoot, mu: sync.Mutex{}, containers: map[string]*container{}}
}

func (s *service) Create(_ context.Context, req *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	if err := validIDs(req.GetID(), ""); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, fmt.Errorf("create crun root: %w", err)
	}

	p, err := newProcess(req.GetTerminal(), req.GetStdin() != "", req.GetStdout() != "", req.GetStderr() != "")
	if err != nil {
		return nil, err
	}

	pid, err := s.startProcess(p, []string{"create", "--bundle", req.GetBundle()}, req.GetID(), "")
	if err != nil {
		s.removeFiles(req.GetID(), "")

		return nil, status.Error(codes.FailedPrecondition, err.Error()) //nolint:wrapcheck // A ttrpc status carries the message to containerd.
	}

	s.mu.Lock()
	s.containers[req.GetID()] = &container{bundle: req.GetBundle(), init: p, started: false, execs: map[string]*process{}}
	s.mu.Unlock()

	s.log.Info("created", "container", req.GetID(), "pid", pid)

	return &taskAPI.CreateTaskResponse{Pid: uint32(pid)}, nil //nolint:gosec // Pids fit.
}

// startProcess runs a crun create or exec command and returns the new process's pid.
func (s *service) startProcess(p *process, args []string, id, execID string) (int, error) {
	files := s.files(id, execID)
	args = append(args, "--pid-file", files.pid)

	var consoleDone chan error

	if p.terminal {
		sock := files.console
		_ = os.Remove(sock)

		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			return 0, fmt.Errorf("listen on console socket: %w", err)
		}
		defer l.Close() //nolint:errcheck // Only used to receive one descriptor.

		consoleDone = make(chan error, 1)

		go func() {
			f, err := receiveConsole(l)
			consoleDone <- err

			p.setConsole(f)
		}()

		args = append(args, "--console-socket", sock)
	}

	stdio, closeNull, err := p.stdio()
	if err != nil {
		return 0, err
	}

	err = s.crunCmd(stdio, append(args, id)...)

	closeNull()
	p.started()

	if consoleDone != nil {
		if err == nil {
			err = <-consoleDone
		} else {
			p.setConsole(nil) // crun never connected, so attached streams end.
		}
	}

	if err != nil {
		p.closeAll()

		return 0, err
	}

	pid, err := readPid(files.pid)
	if err != nil {
		return 0, err
	}

	p.watch(pid, s.reaper.watch(pid), s.log)

	return pid, nil
}

func (s *service) Start(_ context.Context, req *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	c, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	if req.GetExecID() == "" {
		err = s.crunCmd([3]*os.File{}, "start", req.GetID())
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error()) //nolint:wrapcheck // See Create.
		}

		s.mu.Lock()
		c.started = true
		s.mu.Unlock()

		pid, _, _ := p.pidAndExit()
		s.log.Info("started", "container", req.GetID(), "pid", pid)

		return &taskAPI.StartResponse{Pid: uint32(pid)}, nil //nolint:gosec // Pids fit.
	}

	pid, err := s.startProcess(p, []string{"exec", "--detach", "--process", s.files(req.GetID(), req.GetExecID()).spec}, req.GetID(), req.GetExecID())
	if err != nil {
		s.removeFiles(req.GetID(), req.GetExecID())

		return nil, status.Error(codes.FailedPrecondition, err.Error()) //nolint:wrapcheck // See Create.
	}

	return &taskAPI.StartResponse{Pid: uint32(pid)}, nil //nolint:gosec // Pids fit.
}

func (s *service) Exec(_ context.Context, req *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	if err := validIDs(req.GetID(), req.GetExecID()); err != nil {
		return nil, err
	}

	s.mu.Lock()
	c, ok := s.containers[req.GetID()]
	exists := ok && c.execs[req.GetExecID()] != nil
	s.mu.Unlock()

	if !ok {
		return nil, notFound(req.GetID(), "")
	}

	if exists {
		return nil, status.Errorf(codes.AlreadyExists, "exec %s already exists", req.GetExecID())
	}

	p, err := newProcess(req.GetTerminal(), req.GetStdin() != "", req.GetStdout() != "", req.GetStderr() != "")
	if err != nil {
		return nil, err
	}

	// The spec is a runtime-spec Process encoded as JSON.
	err = os.WriteFile(s.files(req.GetID(), req.GetExecID()).spec, req.GetSpec().GetValue(), 0o600)
	if err != nil {
		p.closeAll()

		return nil, fmt.Errorf("write exec spec: %w", err)
	}

	s.mu.Lock()
	c.execs[req.GetExecID()] = p
	s.mu.Unlock()

	return &emptypb.Empty{}, nil
}

// processFiles are the agent's files of one process in the crun root.
type processFiles struct{ spec, pid, console string }

func (s *service) files(id, execID string) processFiles {
	base := filepath.Join(s.root, id+"-"+execID)

	// A socket path must stay under 108 bytes, and the ids are up to 64 bytes each.
	sum := sha256.Sum256([]byte(id + "/" + execID))

	return processFiles{spec: base + ".json", pid: base + ".pid", console: filepath.Join(s.root, hex.EncodeToString(sum[:8])+".sock")}
}

// removeFiles removes the agent's files of a process, so that repeated exec probes leave nothing behind.
func (s *service) removeFiles(id, execID string) {
	f := s.files(id, execID)
	for _, path := range []string{f.spec, f.pid, f.console} {
		_ = os.Remove(path)
	}
}

// validIDs rejects ids that would leave the crun root as file names.
func validIDs(id, execID string) error {
	for _, v := range []string{id, execID} {
		if strings.ContainsAny(v, "/\x00") || v == "." || v == ".." {
			return status.Errorf(codes.InvalidArgument, "id %q is not valid", v)
		}
	}

	if id == "" {
		return status.Error(codes.InvalidArgument, "empty container id") //nolint:wrapcheck // ttrpc status.
	}

	return nil
}

func (s *service) Wait(ctx context.Context, req *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	_, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // The caller went away.
	}

	_, e, _ := p.pidAndExit()

	return &taskAPI.WaitResponse{ExitStatus: e.Status, ExitedAt: timestamppb.New(e.At)}, nil
}

func (s *service) Kill(_ context.Context, req *taskAPI.KillRequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	pid, _, exited := p.pidAndExit()
	if exited || pid == 0 {
		return &emptypb.Empty{}, nil
	}

	if req.GetExecID() == "" {
		args := []string{"kill"}
		if req.GetAll() {
			args = append(args, "--all")
		}

		err = s.crunCmd([3]*os.File{}, append(args, req.GetID(), strconv.FormatUint(uint64(req.GetSignal()), 10))...)
	} else {
		err = unix.Kill(pid, syscall.Signal(req.GetSignal()))
	}

	if err != nil {
		return nil, fmt.Errorf("kill %s: %w", req.GetID(), err)
	}

	return &emptypb.Empty{}, nil
}

func (s *service) Delete(_ context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	c, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	s.removeFiles(req.GetID(), req.GetExecID())

	if req.GetExecID() != "" {
		s.mu.Lock()
		delete(c.execs, req.GetExecID())
		s.mu.Unlock()
	} else {
		err = s.crunCmd([3]*os.File{}, "delete", "--force", req.GetID())
		if err != nil {
			s.log.Warn("crun delete", "container", req.GetID(), "error", err)
		}

		s.mu.Lock()
		delete(s.containers, req.GetID())
		s.mu.Unlock()
	}

	pid, e, _ := p.pidAndExit()
	p.closeAll()

	return &taskAPI.DeleteResponse{Pid: uint32(pid), ExitStatus: e.Status, ExitedAt: timestamppb.New(e.At)}, nil //nolint:gosec // Pids fit.
}

func (s *service) State(_ context.Context, req *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	c, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	pid, e, exited := p.pidAndExit()

	s.mu.Lock()
	started := c.started || req.GetExecID() != ""
	s.mu.Unlock()

	st := task.Status_CREATED

	switch {
	case exited:
		st = task.Status_STOPPED
	case started && pid != 0:
		st = task.Status_RUNNING
	}

	return &taskAPI.StateResponse{
		ID: req.GetID(), ExecID: req.GetExecID(), Bundle: c.bundle, Pid: uint32(pid), Status: st, //nolint:gosec // Pids fit.
		Terminal: p.terminal, ExitStatus: e.Status, ExitedAt: timestamppb.New(e.At),
	}, nil
}

func (s *service) Pids(_ context.Context, req *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	c, p, err := s.find(req.GetID(), "")
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	execs := slices.Collect(maps.Values(c.execs))
	s.mu.Unlock()

	all := append([]*process{p}, execs...)

	procs := make([]*task.ProcessInfo, 0, len(all))

	for _, q := range all {
		if pid, _, exited := q.pidAndExit(); pid != 0 && !exited {
			procs = append(procs, &task.ProcessInfo{Pid: uint32(pid)}) //nolint:gosec // Pids fit.
		}
	}

	return &taskAPI.PidsResponse{Processes: procs}, nil
}

func (s *service) CloseIO(_ context.Context, req *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	if req.GetStdin() && p.stdin != nil {
		_ = p.stdin.Close()
	}

	return &emptypb.Empty{}, nil
}

func (s *service) ResizePty(_ context.Context, req *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	if !p.terminal {
		return &emptypb.Empty{}, nil
	}

	err = p.resize(winsize{rows: uint16(req.GetHeight()), cols: uint16(req.GetWidth())}) //nolint:gosec // Terminal sizes fit.
	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *service) Connect(_ context.Context, req *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	var pid int

	if _, p, err := s.find(req.GetID(), ""); err == nil {
		pid, _, _ = p.pidAndExit()
	}

	return &taskAPI.ConnectResponse{ShimPid: uint32(os.Getpid()), TaskPid: uint32(pid)}, nil //nolint:gosec // Pids fit.
}

// Shutdown kills every container and ends the VM. The response goes out before the VM stops.
func (s *service) Shutdown(_ context.Context, _ *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.containers))
	s.mu.Unlock()

	for _, id := range ids {
		_ = s.crunCmd([3]*os.File{}, "kill", "--all", id, "KILL")
	}

	unix.Sync()
	s.log.Info("powering off")

	time.AfterFunc(100*time.Millisecond, func() { _ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART) })

	return &emptypb.Empty{}, nil
}

func (s *service) Pause(context.Context, *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	return nil, unimplemented("pause")
}

func (s *service) Resume(context.Context, *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	return nil, unimplemented("resume")
}

func (s *service) Checkpoint(context.Context, *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, unimplemented("checkpoint")
}

func (s *service) Update(context.Context, *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil // The guest keeps the limits from the spec.
}

func (s *service) Stats(context.Context, *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return nil, unimplemented("stats")
}

// find returns a container and one of its processes: the init process for an empty execID.
func (s *service) find(id, execID string) (*container, *process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers[id]
	if !ok {
		return nil, nil, notFound(id, execID)
	}

	if execID == "" {
		return c, c.init, nil
	}

	p, ok := c.execs[execID]
	if !ok {
		return nil, nil, notFound(id, execID)
	}

	return c, p, nil
}

// serveStreams carries stdio and port-forward streams from the host.
func (s *service) serveStreams(l net.Listener) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			s.log.Error("stream accept", "error", err)

			continue
		}

		go s.serveStream(conn)
	}
}

func (s *service) serveStream(conn net.Conn) {
	in := bufio.NewReader(conn)

	h, err := podvm.ReadHeader(in)
	if err != nil {
		s.log.Warn("stream", "error", err)

		_ = conn.Close()

		return
	}

	if h.Kind == podvm.PortForward {
		forward(conn, in, h.Port)

		return
	}

	_, p, err := s.find(h.Container, h.Exec)
	if err != nil {
		s.log.Warn("stream for unknown process", "container", h.Container, "exec", h.Exec)

		_ = conn.Close()

		return
	}

	p.attach(h.Kind, conn, in)
}

// forward connects a port-forward stream to a TCP port in the guest.
func forward(conn net.Conn, in io.Reader, port uint16) {
	defer conn.Close() //nolint:errcheck // The stream ends either way.

	var d net.Dialer

	target, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		target, err = d.DialContext(context.Background(), "tcp", net.JoinHostPort("::1", strconv.Itoa(int(port))))
	}

	if err != nil {
		return
	}

	defer target.Close() //nolint:errcheck // The stream ends either way.

	done := make(chan struct{}, 2)

	go func() { _, _ = io.Copy(target, in); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, target); done <- struct{}{} }()

	<-done
}

func newCmd(path string, args []string, stdio [3]*os.File) *exec.Cmd {
	cmd := exec.Command(path, args...) //nolint:noctx // The reaper waits, not a context.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio[0], stdio[1], stdio[2]

	return cmd
}

func notFound(id, execID string) error {
	return status.Errorf(codes.NotFound, "process %s/%s not found", id, execID)
}

func unimplemented(what string) error {
	return status.Error(codes.Unimplemented, what+" is not supported in sbxr pods") //nolint:wrapcheck // ttrpc status.
}
