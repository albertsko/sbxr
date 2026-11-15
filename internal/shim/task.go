//go:build linux

package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/albertsko/sbxr/internal/podvm"
)

// task is a container of the pod.
type task struct {
	id   string
	init *proc

	mu    sync.Mutex
	execs map[string]*proc
}

// proc is a process of a task, as the host sees it: its stdio and its exit.
type proc struct {
	id, exec string
	pid      uint32
	io       *procIO

	once   sync.Once
	done   chan struct{}
	status uint32
	at     time.Time
}

func newProc(id, exec string, io *procIO) *proc {
	return &proc{id: id, exec: exec, pid: 0, io: io, once: sync.Once{}, done: make(chan struct{}), status: 0, at: time.Time{}}
}

// finish records the exit once.
func (p *proc) finish(status uint32, at time.Time) bool {
	first := false

	p.once.Do(func() {
		p.status, p.at, first = status, at, true
		close(p.done)
	})

	return first
}

func (t *task) processes() []*proc {
	t.mu.Lock()
	defer t.mu.Unlock()

	all := make([]*proc, 0, 1+len(t.execs))
	all = append(all, t.init)

	for _, p := range t.execs {
		all = append(all, p)
	}

	return all
}

func (s *service) Create(ctx context.Context, req *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	sb, err := s.getSandbox("")
	if err != nil {
		return nil, err
	}

	id := req.GetID()
	share := filepath.Join(sb.share(), id)
	guestBundle := filepath.Join(podvm.GuestShare, id)

	err = prepareBundle(sb, req, share, guestBundle)
	if err != nil {
		s.cleanupTask(sb, id)

		return nil, err
	}

	resp, err := sb.agent.Create(ctx, &taskAPI.CreateTaskRequest{
		ID: id, Bundle: guestBundle, Terminal: req.GetTerminal(),
		Stdin: req.GetStdin(), Stdout: req.GetStdout(), Stderr: req.GetStderr(),
	})
	if err != nil {
		s.cleanupTask(sb, id)

		return nil, err //nolint:wrapcheck // The agent's status goes to containerd unchanged.
	}

	io, err := connectIO(sb.streamSocket(), id, "", req.GetStdin(), req.GetStdout(), req.GetStderr(), req.GetTerminal())
	if err != nil {
		s.cleanupTask(sb, id)

		return nil, err
	}

	t := &task{id: id, init: newProc(id, "", io), mu: sync.Mutex{}, execs: map[string]*proc{}}
	t.init.pid = resp.GetPid()

	s.mu.Lock()
	s.tasks[id] = t
	s.mu.Unlock()

	s.pub.publish("/tasks/create", &eventtypes.TaskCreate{ContainerID: id, Bundle: req.GetBundle(), Rootfs: req.GetRootfs(), Pid: resp.GetPid()})

	return &taskAPI.CreateTaskResponse{Pid: resp.GetPid()}, nil
}

// prepareBundle mounts the container's root file system and volumes into the pod share and writes the guest spec.
func prepareBundle(sb *sandbox, req *taskAPI.CreateTaskRequest, share, guestBundle string) error {
	data, err := os.ReadFile(filepath.Join(req.GetBundle(), "config.json"))
	if err != nil {
		return fmt.Errorf("read container spec: %w", err)
	}

	var spec specs.Spec

	err = json.Unmarshal(data, &spec)
	if err != nil {
		return fmt.Errorf("decode container spec: %w", err)
	}

	err = mountRootfs(req.GetRootfs(), filepath.Join(share, "rootfs"), filepath.Join(sb.dir, "layers", req.GetID()), sb.usernsFD, sb.uid)
	if err != nil {
		return err
	}

	for _, sh := range guestSpec(&spec, req.GetID(), guestBundle) {
		err := shareFile(sh.Source, filepath.Join(share, sh.Name), sb.usernsFD, sh.ReadOnly)
		if err != nil {
			return err
		}
	}

	data, err = json.Marshal(spec)
	if err == nil {
		err = os.WriteFile(filepath.Join(share, "config.json"), data, 0o644) //nolint:gosec // crun in the guest reads it.
	}

	if err != nil {
		return fmt.Errorf("write guest spec: %w", err)
	}

	return nil
}

// cleanupTask removes the container's mounts and files from the host.
func (s *service) cleanupTask(sb *sandbox, id string) {
	for _, dir := range []string{filepath.Join(sb.share(), id), filepath.Join(sb.dir, "layers", id)} {
		if err := unmountAll(dir); err != nil {
			s.log.Warn("unmount", "container", id, "error", err)
		}

		_ = os.RemoveAll(dir)
	}
}

func (s *service) Start(ctx context.Context, req *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	sb, t, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	resp, err := sb.agent.Start(ctx, req)
	if err != nil {
		return nil, err //nolint:wrapcheck // The agent's status goes to containerd unchanged.
	}

	p.pid = resp.GetPid()

	if req.GetExecID() == "" {
		s.pub.publish("/tasks/start", &eventtypes.TaskStart{ContainerID: t.id, Pid: p.pid})
	} else {
		s.pub.publish("/tasks/exec-started", &eventtypes.TaskExecStarted{ContainerID: t.id, ExecID: p.exec, Pid: p.pid})
	}

	go s.monitor(sb, p) //nolint:gosec // The process outlives the Start request.

	return resp, nil
}

// monitor waits for the process in the guest, lets its output drain, and reports the exit.
func (s *service) monitor(sb *sandbox, p *proc) {
	resp, err := sb.agent.Wait(context.Background(), &taskAPI.WaitRequest{ID: p.id, ExecID: p.exec})

	exitStatus, at := uint32(137), time.Now()
	if err == nil {
		exitStatus, at = resp.GetExitStatus(), resp.GetExitedAt().AsTime()
	}

	p.io.drain(2 * time.Second)
	s.exited(p, exitStatus, at)
}

// exited records a process's exit and tells containerd, once.
func (s *service) exited(p *proc, exitStatus uint32, at time.Time) {
	if p.finish(exitStatus, at) {
		s.pub.publish("/tasks/exit", &eventtypes.TaskExit{ContainerID: p.id, ID: idOr(p.exec, p.id), Pid: p.pid, ExitStatus: exitStatus, ExitedAt: timestamppb.New(at)})
	}
}

func (s *service) Exec(ctx context.Context, req *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	sb, t, _, err := s.find(req.GetID(), "")
	if err != nil {
		return nil, err
	}

	resp, err := sb.agent.Exec(ctx, req)
	if err != nil {
		return nil, err //nolint:wrapcheck // The agent's status goes to containerd unchanged.
	}

	io, err := connectIO(sb.streamSocket(), t.id, req.GetExecID(), req.GetStdin(), req.GetStdout(), req.GetStderr(), req.GetTerminal())
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.execs[req.GetExecID()] = newProc(t.id, req.GetExecID(), io)
	t.mu.Unlock()

	s.pub.publish("/tasks/exec-added", &eventtypes.TaskExecAdded{ContainerID: t.id, ExecID: req.GetExecID()})

	return resp, nil
}

func (s *service) Wait(ctx context.Context, req *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	_, _, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // containerd gave up.
	}

	return &taskAPI.WaitResponse{ExitStatus: p.status, ExitedAt: timestamppb.New(p.at)}, nil
}

func (s *service) Delete(ctx context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	sb, t, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	resp, err := sb.agent.Delete(ctx, req)
	if err != nil {
		// The VM may be gone. The host side still goes.
		resp = &taskAPI.DeleteResponse{Pid: p.pid, ExitStatus: p.status, ExitedAt: timestamppb.New(p.at)}
	}

	p.io.close()

	if req.GetExecID() != "" {
		t.mu.Lock()
		delete(t.execs, req.GetExecID())
		t.mu.Unlock()

		return resp, nil
	}

	s.mu.Lock()
	delete(s.tasks, t.id)
	s.mu.Unlock()

	s.cleanupTask(sb, t.id)
	s.pub.publish("/tasks/delete", &eventtypes.TaskDelete{ContainerID: t.id, ID: t.id, Pid: resp.GetPid(), ExitStatus: resp.GetExitStatus(), ExitedAt: resp.GetExitedAt()})

	return resp, nil
}

func (s *service) Kill(ctx context.Context, req *taskAPI.KillRequest) (*emptypb.Empty, error) {
	sb, _, _, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	return sb.agent.Kill(ctx, req) //nolint:wrapcheck // The agent's status goes to containerd unchanged.
}

func (s *service) CloseIO(_ context.Context, req *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	_, _, p, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	if req.GetStdin() {
		p.io.closeStdin()
	}

	return &emptypb.Empty{}, nil
}

func (s *service) ResizePty(ctx context.Context, req *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	sb, _, _, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	return sb.agent.ResizePty(ctx, req) //nolint:wrapcheck // See Kill.
}

func (s *service) State(ctx context.Context, req *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	sb, _, _, err := s.find(req.GetID(), req.GetExecID())
	if err != nil {
		return nil, err
	}

	return sb.agent.State(ctx, req) //nolint:wrapcheck // See Kill.
}

func (s *service) Pids(ctx context.Context, req *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	sb, _, _, err := s.find(req.GetID(), "")
	if err != nil {
		return nil, err
	}

	return sb.agent.Pids(ctx, req) //nolint:wrapcheck // See Kill.
}

func (s *service) Update(ctx context.Context, req *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	sb, _, _, err := s.find(req.GetID(), "")
	if err != nil {
		return nil, err
	}

	return sb.agent.Update(ctx, req) //nolint:wrapcheck // See Kill.
}

func (s *service) Stats(ctx context.Context, req *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	sb, _, _, err := s.find(req.GetID(), "")
	if err != nil {
		return nil, err
	}

	return sb.agent.Stats(ctx, req) //nolint:wrapcheck // See Kill.
}

func (s *service) Connect(_ context.Context, req *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	resp := &taskAPI.ConnectResponse{ShimPid: uint32(os.Getpid())} //nolint:gosec // Pids fit.

	if _, _, p, err := s.find(req.GetID(), ""); err == nil {
		resp.TaskPid = p.pid
	}

	return resp, nil
}

// Shutdown is a no-op: the sandbox API ends this shim.
func (s *service) Shutdown(context.Context, *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *service) Pause(context.Context, *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "pause is not supported") //nolint:wrapcheck // ttrpc status.
}

func (s *service) Resume(context.Context, *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "resume is not supported") //nolint:wrapcheck // ttrpc status.
}

func (s *service) Checkpoint(context.Context, *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented, "checkpoint is not supported") //nolint:wrapcheck // ttrpc status.
}

// find returns the sandbox, a task, and one of its processes: the init process for an empty execID.
func (s *service) find(id, execID string) (*sandbox, *task, *proc, error) {
	s.mu.Lock()
	sb, t := s.sandbox, s.tasks[id]
	s.mu.Unlock()

	if sb == nil || sb.agent == nil || t == nil {
		return nil, nil, nil, status.Errorf(codes.NotFound, "task %s not found", id)
	}

	if execID == "" {
		return sb, t, t.init, nil
	}

	t.mu.Lock()
	p, ok := t.execs[execID]
	t.mu.Unlock()

	if !ok {
		return nil, nil, nil, status.Errorf(codes.NotFound, "exec %s/%s not found", id, execID)
	}

	return sb, t, p, nil
}

func idOr(id, fallback string) string {
	if id == "" {
		return fallback
	}

	return id
}
