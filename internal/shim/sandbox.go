//go:build linux

package shim

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/albertsko/sbxr/internal/podnet"
	"github.com/albertsko/sbxr/internal/podvm"
)

// Install paths from the node installer.
const (
	libDir    = "/opt/sbxr/lib"
	guestRoot = "/opt/sbxr/guest"
	runDir    = "/run/sbxr"
	podsDir   = runDir + "/pods"
)

// bootTimeout bounds the time from the VM start until the guest agent answers.
const bootTimeout = 30 * time.Second

// sandbox is the pod VM. One shim process serves one sandbox and its containers.
type sandbox struct {
	id     string
	netns  string
	config podConfig
	dir    string // Pod directory on the host.
	uid    int    // First host id of the VM's user namespace.

	vm       *vmProcess
	usernsFD int
	agent    taskAPI.TTRPCTaskService
	conn     *ttrpc.Client
	portFwd  net.Listener

	createdAt time.Time
	exitedAt  time.Time
}

func (s *sandbox) share() string         { return filepath.Join(s.dir, "share") }
func (s *sandbox) controlSocket() string { return filepath.Join(s.dir, "sock", "control") }
func (s *sandbox) streamSocket() string  { return filepath.Join(s.dir, "sock", "stream") }

// CreateSandbox records the pod. StartSandbox does the work, because containerd may retry it.
func (s *service) CreateSandbox(_ context.Context, req *sandboxAPI.CreateSandboxRequest) (*sandboxAPI.CreateSandboxResponse, error) {
	cfg, err := parsePodConfig(req.GetOptions().GetValue())
	if err != nil {
		return nil, err
	}

	uid, err := allocateIDs(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	sb := &sandbox{id: req.GetSandboxID(), netns: req.GetNetnsPath(), config: cfg, dir: filepath.Join(podsDir, req.GetSandboxID()), uid: uid, usernsFD: -1, vm: nil, agent: nil, conn: nil, portFwd: nil, createdAt: time.Time{}, exitedAt: time.Time{}}

	err = makeTraversable(sb.dir)
	if err == nil {
		err = os.WriteFile(filepath.Join(sb.dir, "ids"), []byte(strconv.Itoa(uid)), 0o600)
	}

	if err != nil {
		releaseIDs(uid)

		return nil, fmt.Errorf("create pod directory: %w", err)
	}

	s.mu.Lock()
	s.sandbox = sb
	s.mu.Unlock()

	s.log.Info("sandbox created", "id", sb.id, "uid", uid, "netns", sb.netns)

	return &sandboxAPI.CreateSandboxResponse{}, nil
}

func (s *service) StartSandbox(ctx context.Context, req *sandboxAPI.StartSandboxRequest) (*sandboxAPI.StartSandboxResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	err = s.boot(ctx, sb)
	if err != nil {
		s.log.Error("sandbox start failed", "id", sb.id, "error", err)
		s.stopVM(sb, 0)

		return nil, err
	}

	sb.createdAt = time.Now()

	go s.watchVM(sb)

	return &sandboxAPI.StartSandboxResponse{Pid: uint32(sb.vm.cmd.Process.Pid), CreatedAt: timestamppb.New(sb.createdAt)}, nil //nolint:gosec // Pids fit.
}

// boot prepares the pod directory and network, starts the VM, and waits for the agent.
func (s *service) boot(ctx context.Context, sb *sandbox) error {
	for _, d := range []string{sb.share(), filepath.Dir(sb.controlSocket())} {
		err := os.MkdirAll(d, 0o755) //nolint:gosec // The VM process reads it as another user.
		if err == nil {
			err = os.Chown(d, sb.uid, sb.uid)
		}

		if err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	err := writePodFiles(sb)
	if err != nil {
		return err
	}

	guest, err := podnet.Setup(sb.netns, sb.uid, sb.uid)
	if err != nil {
		return fmt.Errorf("pod network: %w", err)
	}

	err = podvm.WritePod(sb.share(), podvm.Pod{Hostname: sb.config.Hostname, Addresses: guest.Addresses, Routes: guest.Routes, MTU: guest.MTU})
	if err != nil {
		return err //nolint:wrapcheck // WritePod names the file.
	}

	sb.portFwd, err = s.listenPortForward(sb)
	if err != nil {
		s.log.Warn("kubectl port-forward will not work", "error", err)
	}

	log, err := os.OpenFile(filepath.Join(sb.dir, "vm.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open VM log: %w", err)
	}
	defer log.Close() //nolint:errcheck // The VM process has its own copy.

	m := machine{
		LibDir: libDir, GuestRoot: guestRoot, Share: sb.share(),
		ControlSocket: sb.controlSocket(), StreamSocket: sb.streamSocket(),
		TAP: podnet.TAP, MAC: guest.MAC.String(),
		CPUs: vmCPUs(sb.config), MemoryMiB: vmMemoryMiB(sb.config), LogLevel: 0,
	}

	sb.vm, err = startVM(sb.dir, m, sb.netns, sb.uid, log)
	if err != nil {
		return err
	}

	pid := sb.vm.cmd.Process.Pid
	joinCgroup(sb.config.CgroupParent, pid, s.log)

	// The delete action kills the VM by this pid when the shim died.
	_ = os.WriteFile(filepath.Join(sb.dir, "vm.pid"), []byte(strconv.Itoa(pid)), 0o600)

	sb.usernsFD, err = unix.Open(fmt.Sprintf("/proc/%d/ns/user", pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		// The VM process may have failed already. Its log says why.
		select {
		case <-sb.vm.done:
			return fmt.Errorf("the VM stopped at once (%v); %s", vmExit(sb.vm), tail(filepath.Join(sb.dir, "vm.log")))
		case <-time.After(time.Second):
			return fmt.Errorf("open VM user namespace: %w", err)
		}
	}

	return s.connectAgent(ctx, sb)
}

// connectAgent waits until the guest agent answers, or the boot timeout passes, or the VM exits.
func (s *service) connectAgent(ctx context.Context, sb *sandbox) error {
	deadline := time.Now().Add(bootTimeout)

	for {
		var d net.Dialer

		dctx, dcancel := context.WithTimeout(ctx, time.Second)
		conn, err := d.DialContext(dctx, "unix", sb.controlSocket())

		dcancel()

		if err == nil {
			client := ttrpc.NewClient(conn)
			agent := taskAPI.NewTTRPCTaskClient(client)

			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err = agent.Connect(callCtx, &taskAPI.ConnectRequest{})

			cancel()

			if err == nil {
				sb.agent, sb.conn = agent, client
				s.log.Info("guest agent connected", "id", sb.id)

				return nil
			}

			_ = client.Close()
		}

		select {
		case <-sb.vm.done:
			return fmt.Errorf("the VM stopped before the guest agent started (%v); %s", vmExit(sb.vm), tail(filepath.Join(sb.dir, "vm.log")))
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // containerd gave up.
		case <-time.After(200 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the guest agent did not start within %s; %s", bootTimeout, tail(filepath.Join(sb.dir, "vm.log")))
		}
	}
}

// watchVM ends every task when the VM exits.
func (s *service) watchVM(sb *sandbox) {
	<-sb.vm.done

	s.mu.Lock()
	sb.exitedAt = time.Now()
	tasks := make([]*task, 0, len(s.tasks))

	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()

	s.log.Info("VM exited", "id", sb.id, "status", sb.vm.exit)

	for _, t := range tasks {
		for _, p := range t.processes() {
			s.exited(p, 137, sb.exitedAt)
		}
	}
}

func (s *service) StopSandbox(_ context.Context, req *sandboxAPI.StopSandboxRequest) (*sandboxAPI.StopSandboxResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(req.GetTimeoutSecs()) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	s.stopVM(sb, timeout)

	return &sandboxAPI.StopSandboxResponse{}, nil
}

// stopVM asks the agent to power off, then kills the VM process after timeout.
func (s *service) stopVM(sb *sandbox, timeout time.Duration) {
	if sb.vm == nil {
		return
	}

	if sb.agent != nil && timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = sb.agent.Shutdown(ctx, &taskAPI.ShutdownRequest{})

		cancel()
	}

	select {
	case <-sb.vm.done:
	case <-time.After(timeout):
		_ = sb.vm.cmd.Process.Kill()
		<-sb.vm.done
	}
}

func (s *service) WaitSandbox(ctx context.Context, req *sandboxAPI.WaitSandboxRequest) (*sandboxAPI.WaitSandboxResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	if sb.vm == nil {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is not started") //nolint:wrapcheck // ttrpc status.
	}

	select {
	case <-sb.vm.done:
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // containerd gave up.
	}

	code := uint32(0)

	if exitErr, ok := errors.AsType[*exec.ExitError](sb.vm.exit); ok {
		code = uint32(exitErr.ExitCode()) //nolint:gosec // Exit codes fit.
	}

	return &sandboxAPI.WaitSandboxResponse{ExitStatus: code, ExitedAt: timestamppb.New(sb.exitedAt)}, nil
}

func (s *service) SandboxStatus(_ context.Context, req *sandboxAPI.SandboxStatusRequest) (*sandboxAPI.SandboxStatusResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	resp := &sandboxAPI.SandboxStatusResponse{SandboxID: sb.id, State: "SANDBOX_NOTREADY", CreatedAt: timestamppb.New(sb.createdAt)}

	if sb.vm != nil {
		resp.Pid = uint32(sb.vm.cmd.Process.Pid) //nolint:gosec // Pids fit.

		select {
		case <-sb.vm.done:
			resp.ExitedAt = timestamppb.New(sb.exitedAt)
		default:
			resp.State = "SANDBOX_READY"
		}
	}

	return resp, nil
}

func (s *service) PingSandbox(context.Context, *sandboxAPI.PingRequest) (*sandboxAPI.PingResponse, error) {
	return &sandboxAPI.PingResponse{}, nil
}

// ShutdownSandbox removes everything of the pod and ends this shim.
func (s *service) ShutdownSandbox(_ context.Context, req *sandboxAPI.ShutdownSandboxRequest) (*sandboxAPI.ShutdownSandboxResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	s.stopVM(sb, time.Second)
	cleanupPod(sb.dir)

	if sb.portFwd != nil {
		_ = sb.portFwd.Close()
	}

	s.log.Info("sandbox shut down", "id", sb.id)

	// Answer first, then end the shim, as containerd expects.
	time.AfterFunc(100*time.Millisecond, func() { s.shutdown() })

	return &sandboxAPI.ShutdownSandboxResponse{}, nil
}

// vmExit describes how the VM process ended.
func vmExit(vm *vmProcess) string {
	if vm.exit == nil {
		return "exit status 0"
	}

	return vm.exit.Error()
}

// makeTraversable creates dir and lets every user pass through it and its parents up to /run/sbxr.
// The VM process runs as another user and must reach its pod directory.
func makeTraversable(dir string) error {
	err := os.MkdirAll(dir, 0o711) //nolint:gosec // See above.
	if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	for d := dir; d != filepath.Dir(runDir); d = filepath.Dir(d) {
		err = os.Chmod(d, 0o711) //nolint:gosec // Only traversal, no listing.
		if err != nil {
			return fmt.Errorf("make %s traversable: %w", d, err)
		}
	}

	return nil
}

// cleanupPod unmounts and removes a pod directory and releases its id range.
func cleanupPod(dir string) {
	if data, err := os.ReadFile(filepath.Join(dir, "ids")); err == nil {
		if uid, err := strconv.Atoi(string(data)); err == nil {
			releaseIDs(uid)
		}
	}

	_ = unmountAll(dir)
	_ = os.RemoveAll(dir)
}

func (s *service) Platform(context.Context, *sandboxAPI.PlatformRequest) (*sandboxAPI.PlatformResponse, error) {
	return &sandboxAPI.PlatformResponse{Platform: &types.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
}

func (s *service) SandboxMetrics(context.Context, *sandboxAPI.SandboxMetricsRequest) (*sandboxAPI.SandboxMetricsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "sandbox metrics are not supported") //nolint:wrapcheck // ttrpc status.
}

func (s *service) getSandbox(id string) (*sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sandbox == nil || (id != "" && s.sandbox.id != id) {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", id)
	}

	return s.sandbox, nil
}

// writePodFiles writes the resolv.conf and hostname that the containers mount.
func writePodFiles(sb *sandbox) error {
	resolv := sb.config.resolvConf()
	if resolv == nil {
		var err error

		resolv, err = os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return fmt.Errorf("read node resolv.conf: %w", err)
		}
	}

	hostname := sb.config.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	err := os.WriteFile(filepath.Join(sb.share(), "resolv.conf"), resolv, 0o644) //nolint:gosec // Containers read it.
	if err == nil {
		err = os.WriteFile(filepath.Join(sb.share(), "hostname"), []byte(hostname+"\n"), 0o644) //nolint:gosec // See above.
	}

	if err != nil {
		return fmt.Errorf("write pod files: %w", err)
	}

	return nil
}

// vmMemoryMiB is the pod's memory limit, which is the sum of its containers' limits, or 1024 MiB without one.
func vmMemoryMiB(c podConfig) uint32 {
	if c.MemoryBytes <= 0 {
		return 1024
	}

	return uint32(max(c.MemoryBytes>>20, 1)) //nolint:gosec // A pod limit in MiB fits.
}

// vmCPUs is the pod's CPU limit rounded up, or every node CPU without one, at most 255.
func vmCPUs(c podConfig) uint8 {
	n := runtime.NumCPU()
	if c.CPUQuota > 0 && c.CPUPeriod > 0 {
		n = min(n, int((c.CPUQuota+c.CPUPeriod-1)/c.CPUPeriod))
	}

	return uint8(min(max(n, 1), 255))
}

// joinCgroup moves pid into a child cgroup of the pod cgroup, so the pod's limits and accounting cover the VM.
func joinCgroup(parent string, pid int, log *slog.Logger) {
	if parent == "" {
		return
	}

	dir := filepath.Clean("/sys/fs/cgroup/" + cgroupPath(parent) + "/sbxr-vm")

	err := os.MkdirAll(dir, 0o755) //nolint:gosec // cgroup directories are world-readable.
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644) //nolint:gosec // cgroup file.
	}

	if err != nil {
		log.Warn("VM process stays outside the pod cgroup", "cgroup", dir, "error", err)
	}
}

// cgroupPath turns a systemd slice name, such as kubepods-burstable-pod1.slice, into its cgroup path.
// A cgroupfs path stays as it is.
func cgroupPath(parent string) string {
	name, ok := strings.CutSuffix(parent, ".slice")
	if !ok || strings.Contains(parent, "/") {
		return parent
	}

	var p, prefix string

	for part := range strings.SplitSeq(name, "-") {
		prefix += part
		p = filepath.Join(p, prefix+".slice")
		prefix += "-"
	}

	return p
}

// tail returns the last lines of a log file for an error message.
func tail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "no VM log"
	}
	defer f.Close() //nolint:errcheck // Read-only.

	var lines []string

	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 5 {
			lines = lines[1:]
		}
	}

	return "VM log: " + strings.Join(lines, " | ")
}
