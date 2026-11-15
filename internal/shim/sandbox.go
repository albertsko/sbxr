//go:build linux

package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Paths in a pod directory.
const (
	shareDir      = "share"
	sockDir       = "sock"
	controlSocket = sockDir + "/control"
	streamSocket  = sockDir + "/stream"
	layersDir     = "layers"
	vmLog         = "vm.log"
	vmPidFile     = "vm.pid"
	idsFile       = "ids"
)

// defaultBootTimeout bounds the time from the VM start until the guest agent answers.
const defaultBootTimeout = 30 * time.Second

// sandbox is the pod VM. One shim process serves one sandbox and its containers.
// The fields after uid change while the sandbox starts and stops. Read and write them under service.mu.
type sandbox struct {
	id     string
	netns  string
	config podConfig
	dir    *podDir
	ids    idPool
	uid    int // First host id of the VM's user namespace, also its group.

	vm        *vmProcess
	usernsFD  int
	agent     taskAPI.TTRPCTaskService
	conn      *ttrpc.Client
	net       *podnet.Guest
	portFwd   *portForward
	createdAt time.Time
	exitedAt  time.Time
}

// dialStream opens a stream to the guest agent.
func (sb *sandbox) dialStream(h podvm.Header) (*net.UnixConn, error) {
	conn, err := sb.dir.dialSocket(streamSocket, sb.uid)
	if err != nil {
		return nil, fmt.Errorf("open %s stream: %w", h.Kind, err)
	}

	return sendHeader(conn, h)
}

// CreateSandbox claims the pod's ids and creates its directory. StartSandbox boots the VM.
func (s *service) CreateSandbox(_ context.Context, req *sandboxAPI.CreateSandboxRequest) (*sandboxAPI.CreateSandboxResponse, error) {
	if err := podvm.ValidID(req.GetSandboxID()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox "+err.Error()) //nolint:wrapcheck // ttrpc status.
	}

	cfg, err := parsePodConfig(req.GetOptions().GetValue())
	if err != nil {
		return nil, err
	}

	ids, err := loadIDPool("/etc", idsDir)
	if err != nil {
		return nil, err
	}

	uid, err := ids.allocate(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	err = makeTraversable(podsDir)

	var dir *podDir
	if err == nil {
		dir, err = openPodDir(filepath.Join(podsDir, req.GetSandboxID()), uid)
	}

	if err == nil {
		err = dir.writeFile(idsFile, []byte(strconv.Itoa(uid)), 0o600)
	}

	if err != nil {
		ids.release(uid)

		return nil, fmt.Errorf("create pod directory: %w", err)
	}

	sb := &sandbox{
		id: req.GetSandboxID(), netns: req.GetNetnsPath(), config: cfg, dir: dir, ids: ids, uid: uid,
		vm: nil, usernsFD: -1, agent: nil, conn: nil, net: nil, portFwd: nil, createdAt: time.Time{}, exitedAt: time.Time{},
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
		s.teardown(sb)

		return nil, err
	}

	s.mu.Lock()
	sb.createdAt = time.Now()
	vm, createdAt := sb.vm, sb.createdAt
	s.mu.Unlock()

	go s.watchVM(sb, vm)

	return &sandboxAPI.StartSandboxResponse{Pid: uint32(vm.cmd.Process.Pid), CreatedAt: timestamppb.New(createdAt)}, nil //nolint:gosec // Pids fit.
}

// boot prepares the pod directory and network, starts the VM, and waits for the agent.
// It records each step in sb, so that teardown can undo it after a failure.
func (s *service) boot(ctx context.Context, sb *sandbox) error {
	// The share is read-only to the VM process, so the guest cannot replace what the shim creates there.
	// The VM process creates its sockets in its own directory.
	err := sb.dir.mkdir(shareDir)
	if err == nil {
		err = sb.dir.mkdirFor(sockDir, sb.uid, 0o700)
	}

	if err == nil {
		err = writePodFiles(sb)
	}

	if err != nil {
		return err
	}

	guest, err := podnet.Setup(sb.netns, sb.uid, sb.uid)
	if err != nil {
		return fmt.Errorf("pod network: %w", err)
	}

	s.mu.Lock()
	sb.net = &guest
	s.mu.Unlock()

	pod, err := json.Marshal(podvm.Pod{Hostname: sb.config.Hostname, Addresses: guest.Addresses, Routes: guest.Routes, MTU: guest.MTU})
	if err == nil {
		err = sb.dir.writeFile(filepath.Join(shareDir, podvm.PodFile), pod, fileMode)
	}

	if err != nil {
		return fmt.Errorf("write pod config: %w", err)
	}

	pf, err := s.listenPortForward(sb)
	if err != nil {
		s.log.Warn("kubectl port-forward will not work", "error", err)
	}

	s.mu.Lock()
	sb.portFwd = pf
	s.mu.Unlock()

	vm, err := s.startPodVM(sb, guest)
	if err != nil {
		return err
	}

	s.mu.Lock()
	sb.vm = vm
	s.mu.Unlock()

	pid := vm.cmd.Process.Pid

	// The delete action kills the VM by this pid when the shim died.
	_ = sb.dir.writeFile(vmPidFile, []byte(strconv.Itoa(pid)), 0o600)

	usernsFD, err := unix.Open(fmt.Sprintf("/proc/%d/ns/user", pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		// The VM process may have failed already. Its log says why.
		select {
		case <-vm.done:
			return fmt.Errorf("the VM stopped at once (%v); %s", vmExit(vm), tail(sb.dir.join(vmLog)))
		case <-time.After(time.Second):
			return fmt.Errorf("open VM user namespace: %w", err)
		}
	}

	s.mu.Lock()
	sb.usernsFD = usernsFD
	s.mu.Unlock()

	agent, conn, err := s.connectAgent(ctx, sb, vm)
	if err != nil {
		return err
	}

	s.mu.Lock()
	sb.agent, sb.conn = agent, conn
	s.mu.Unlock()

	s.log.Info("guest agent connected", "id", sb.id)

	return nil
}

// startPodVM writes the VM config and starts the VM process in the pod cgroup.
func (s *service) startPodVM(sb *sandbox, guest podnet.Guest) (*vmProcess, error) {
	m := machine{
		LibDir: libDir, GuestRoot: guestRoot, Share: sb.dir.join(shareDir),
		ControlSocket: sb.dir.join(controlSocket), StreamSocket: sb.dir.join(streamSocket),
		TAP: podnet.TAP, MAC: guest.MAC.String(),
		CPUs: vmCPUs(sb.config), MemoryMiB: vmMemoryMiB(sb.config), LogLevel: 0,
	}

	data, err := json.Marshal(m)
	if err == nil {
		err = sb.dir.writeFile(machineFile, data, fileMode)
	}

	if err != nil {
		return nil, fmt.Errorf("write VM config: %w", err)
	}

	logFD, err := sb.dir.open(vmLog, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	log := os.NewFile(uintptr(logFD), sb.dir.join(vmLog))
	defer log.Close() //nolint:errcheck // The VM process has its own copy.

	cgroup, err := openVMCgroup(sb.config.CgroupParent)
	if err != nil {
		return nil, err
	}
	defer cgroup.Close() //nolint:errcheck // Only needed to start the VM process.

	return startVM(sb.dir.path, sb.netns, sb.uid, cgroup, log)
}

// connectAgent waits until the guest agent answers, or the boot timeout passes, or the VM exits.
func (s *service) connectAgent(ctx context.Context, sb *sandbox, vm *vmProcess) (taskAPI.TTRPCTaskService, *ttrpc.Client, error) {
	deadline := time.Now().Add(s.bootTimeout)

	for {
		conn, err := sb.dir.dialSocket(controlSocket, sb.uid)
		if err == nil {
			client := ttrpc.NewClient(conn)
			agent := taskAPI.NewTTRPCTaskClient(client)

			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err = agent.Connect(callCtx, &taskAPI.ConnectRequest{})

			cancel()

			if err == nil {
				return agent, client, nil
			}

			_ = client.Close()
		}

		select {
		case <-vm.done:
			return nil, nil, fmt.Errorf("the VM stopped before the guest agent started (%v); %s", vmExit(vm), tail(sb.dir.join(vmLog)))
		case <-ctx.Done():
			return nil, nil, ctx.Err() //nolint:wrapcheck // containerd gave up.
		case <-time.After(200 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("the guest agent did not start within %s; %s", s.bootTimeout, tail(sb.dir.join(vmLog)))
		}
	}
}

// watchVM ends every task when the VM exits.
func (s *service) watchVM(sb *sandbox, vm *vmProcess) {
	<-vm.done

	s.mu.Lock()
	sb.exitedAt = time.Now()
	exitedAt := sb.exitedAt
	tasks := make([]*task, 0, len(s.tasks))

	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()

	s.log.Info("VM exited", "id", sb.id, "status", vmExit(vm))

	for _, t := range tasks {
		for _, p := range t.processes() {
			s.exited(p, 137, exitedAt)
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
	s.mu.Lock()
	vm, agent := sb.vm, sb.agent
	s.mu.Unlock()

	if vm == nil {
		return
	}

	if agent != nil && timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = agent.Shutdown(ctx, &taskAPI.ShutdownRequest{})

		cancel()
	}

	select {
	case <-vm.done:
	case <-time.After(timeout):
		_ = vm.cmd.Process.Kill()
		<-vm.done
	}
}

// teardown undoes what boot did on the host besides the VM itself: the port-forward rule and the pod network.
func (s *service) teardown(sb *sandbox) {
	s.mu.Lock()
	pf, guest, conn := sb.portFwd, sb.net, sb.conn
	sb.portFwd, sb.net = nil, nil
	s.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}

	if pf != nil {
		if err := pf.close(); err != nil {
			s.log.Warn("port-forward cleanup", "id", sb.id, "error", err)
		}
	}

	if guest != nil {
		if err := podnet.Teardown(sb.netns, *guest); err != nil {
			s.log.Warn("pod network cleanup", "id", sb.id, "error", err)
		}
	}
}

func (s *service) WaitSandbox(ctx context.Context, req *sandboxAPI.WaitSandboxRequest) (*sandboxAPI.WaitSandboxResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	vm := sb.vm
	s.mu.Unlock()

	if vm == nil {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is not started") //nolint:wrapcheck // ttrpc status.
	}

	select {
	case <-vm.done:
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // containerd gave up.
	}

	code := uint32(0)

	if exitErr, ok := errors.AsType[*exec.ExitError](vm.exit); ok {
		code = uint32(exitErr.ExitCode()) //nolint:gosec // Exit codes fit.
	}

	s.mu.Lock()
	exitedAt := sb.exitedAt
	s.mu.Unlock()

	return &sandboxAPI.WaitSandboxResponse{ExitStatus: code, ExitedAt: timestamppb.New(exitedAt)}, nil
}

func (s *service) SandboxStatus(_ context.Context, req *sandboxAPI.SandboxStatusRequest) (*sandboxAPI.SandboxStatusResponse, error) {
	sb, err := s.getSandbox(req.GetSandboxID())
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	vm, createdAt, exitedAt := sb.vm, sb.createdAt, sb.exitedAt
	s.mu.Unlock()

	resp := &sandboxAPI.SandboxStatusResponse{SandboxID: sb.id, State: "SANDBOX_NOTREADY", CreatedAt: timestamppb.New(createdAt)}

	if vm != nil {
		resp.Pid = uint32(vm.cmd.Process.Pid) //nolint:gosec // Pids fit.

		select {
		case <-vm.done:
			resp.ExitedAt = timestamppb.New(exitedAt)
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
	s.teardown(sb)

	sb.dir.close()

	if err := cleanupPod(sb.dir.path, sb.ids); err != nil {
		s.log.Warn("pod directory cleanup", "id", sb.id, "error", err)
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

// makeTraversable creates dir and lets every user pass through it and its parents up to /run/sbxr, without listing them.
func makeTraversable(dir string) error {
	err := os.MkdirAll(dir, 0o711) //nolint:gosec // Only traversal, no listing.
	if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	for d := dir; d != filepath.Dir(runDir); d = filepath.Dir(d) {
		err = os.Chmod(d, 0o711) //nolint:gosec // See above.
		if err != nil {
			return fmt.Errorf("make %s traversable: %w", d, err)
		}
	}

	return nil
}

// cleanupPod unmounts and removes a pod directory and releases its id range.
// It keeps the directory and the ids while any mount remains, so that it never removes files of a volume.
func cleanupPod(dir string, ids idPool) error {
	data, readErr := os.ReadFile(filepath.Join(dir, idsFile))

	err := removeTree(dir)
	if err != nil {
		return err
	}

	if uid, err := strconv.Atoi(string(data)); readErr == nil && err == nil {
		ids.release(uid)
	}

	return nil
}

func (s *service) Platform(context.Context, *sandboxAPI.PlatformRequest) (*sandboxAPI.PlatformResponse, error) {
	return &sandboxAPI.PlatformResponse{Platform: &types.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
}

func (s *service) SandboxMetrics(context.Context, *sandboxAPI.SandboxMetricsRequest) (*sandboxAPI.SandboxMetricsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "sandbox metrics are not supported") //nolint:wrapcheck // ttrpc status.
}

// getSandbox returns the sandbox in any state.
func (s *service) getSandbox(id string) (*sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sandbox == nil || (id != "" && s.sandbox.id != id) {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", id)
	}

	return s.sandbox, nil
}

// readySandbox returns the sandbox and its agent once the guest agent answered.
func (s *service) readySandbox() (*sandbox, taskAPI.TTRPCTaskService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sandbox == nil {
		return nil, nil, status.Error(codes.NotFound, "no sandbox") //nolint:wrapcheck // ttrpc status.
	}

	if s.sandbox.agent == nil {
		return nil, nil, status.Error(codes.FailedPrecondition, "the sandbox is not started") //nolint:wrapcheck // ttrpc status.
	}

	return s.sandbox, s.sandbox.agent, nil
}

// writePodFiles writes the resolv.conf and hostname that the containers mount. Every container user may read them.
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

	err := sb.dir.writeFile(filepath.Join(shareDir, "resolv.conf"), resolv, 0o644)
	if err == nil {
		err = sb.dir.writeFile(filepath.Join(shareDir, "hostname"), []byte(hostname+"\n"), 0o644)
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

// openVMCgroup creates the VM's cgroup below the pod cgroup and opens it, so the VM process starts in it.
// The pod's limits and accounting then cover the VM from its first instruction.
func openVMCgroup(parent string) (*os.File, error) {
	if parent == "" {
		return nil, errors.New("the pod has no cgroup parent")
	}

	dir := filepath.Clean("/sys/fs/cgroup/" + cgroupPath(parent) + "/sbxr-vm")

	err := os.Mkdir(dir, 0o755) //nolint:gosec // cgroup directories are world-readable.
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create VM cgroup: %w", err)
	}

	f, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open VM cgroup: %w", err)
	}

	return f, nil
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
