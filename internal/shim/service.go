//go:build linux

// Package shim is containerd-shim-sbxr-v1, the phase 2 containerd runtime (PRD "Kubernetes phase 2").
//
// containerd uses the shim as the pod sandboxer: one shim process serves the sandbox API for one pod and the task API
// for the pod's containers. The shim starts the pod VM as a child process without host capabilities and forwards each
// task request to the guest agent over vsock. It never imports containerd itself, only its API types.
package shim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bootAPI "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	sandboxAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/albertsko/sbxr/internal/podvm"
)

// service implements the containerd sandbox and task APIs for one pod.
type service struct {
	log      *slog.Logger
	pub      *publisher
	shutdown func()

	// bootTimeout bounds the time from the VM start until the guest agent answers.
	bootTimeout time.Duration

	mu      sync.Mutex
	sandbox *sandbox
	tasks   map[string]*task
}

// flags are the command line that containerd passes to every shim action.
type flags struct {
	namespace, address, id, bundle string
	debug                          bool
}

// Main runs one shim action: start, delete, the hidden serve daemon, or the hidden VM process.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == VMArg {
		return RunVM(args[1:], stderr)
	}

	var f flags

	fs := flag.NewFlagSet("containerd-shim-sbxr-v1", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.namespace, "namespace", "", "containerd namespace")
	fs.StringVar(&f.address, "address", "", "containerd GRPC address")
	fs.StringVar(&f.id, "id", "", "sandbox id")
	fs.StringVar(&f.bundle, "bundle", "", "bundle path")
	fs.BoolVar(&f.debug, "debug", false, "debug logs")
	fs.String("publish-binary", "", "containerd binary (unused)")
	info := fs.Bool("info", false, "print the runtime info protobuf")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	var err error

	switch {
	case *info:
		err = printInfo(stdout)
	case fs.Arg(0) == "start":
		err = start(f, stdin, stdout)
	case fs.Arg(0) == "delete":
		err = deleteAction(f, stdout)
	case fs.Arg(0) == "serve":
		err = serve(f)
	default:
		err = fmt.Errorf("unknown action %q", fs.Arg(0))
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "containerd-shim-sbxr-v1: %v\n", err)

		return 1
	}

	return 0
}

// printInfo answers containerd's runtime introspection.
func printInfo(stdout io.Writer) error {
	data, err := proto.Marshal(&types.RuntimeInfo{Name: "io.containerd.sbxr.v1"})
	if err == nil {
		_, err = stdout.Write(data)
	}

	if err != nil {
		return fmt.Errorf("runtime info: %w", err)
	}

	return nil
}

// start launches the shim daemon for a sandbox and prints how to reach it.
func start(f flags, stdin io.Reader, stdout io.Writer) error {
	input, err := io.ReadAll(io.LimitReader(stdin, 10<<20))
	if err != nil {
		return fmt.Errorf("read bootstrap params: %w", err)
	}

	var params bootAPI.BootstrapParams

	bootstrap := len(input) > 0 && proto.Unmarshal(input, &params) == nil
	if bootstrap {
		f.namespace = params.GetNamespace()
		f.id = params.GetInstanceID()
	}

	ttrpcAddress := params.GetContainerdTtrpcAddress()
	if ttrpcAddress == "" {
		ttrpcAddress = os.Getenv("TTRPC_ADDRESS")
	}

	sum := sha256.Sum256([]byte(f.namespace + "/" + f.id))
	sock := "/run/containerd/s/sbxr-" + hex.EncodeToString(sum[:16])

	err = os.MkdirAll(filepath.Dir(sock), 0o711) //nolint:gosec // containerd's socket directory.
	if err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}

	_ = os.Remove(sock) // A leftover from a shim that died.

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}

	lf, err := l.File()
	if err != nil {
		return fmt.Errorf("listener file: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find shim binary: %w", err)
	}

	cmd := exec.Command(self, "-namespace", f.namespace, "-id", f.id, "serve") //nolint:noctx // The daemon outlives this command.
	cmd.ExtraFiles = []*os.File{lf}

	cmd.Env = append(os.Environ(), "TTRPC_ADDRESS="+ttrpcAddress)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start shim daemon: %w", err)
	}

	_ = cmd.Process.Release()

	address := "unix://" + sock
	if bootstrap {
		data, err := proto.Marshal(&bootAPI.BootstrapResult{Version: 3, Address: address, Protocol: "ttrpc"})
		if err != nil {
			return fmt.Errorf("encode bootstrap result: %w", err)
		}

		_, err = stdout.Write(data)

		return err //nolint:wrapcheck // Writing to containerd's pipe.
	}

	return json.NewEncoder(stdout).Encode(map[string]any{"version": 3, "address": address, "protocol": "ttrpc"}) //nolint:wrapcheck // See above.
}

// deleteAction cleans up after a shim that died, and reports the pod as killed.
func deleteAction(f flags, stdout io.Writer) error {
	if err := podvm.ValidID(f.id); err != nil {
		return fmt.Errorf("sandbox %w", err)
	}

	dir := filepath.Join(podsDir, f.id)
	if data, err := os.ReadFile(filepath.Join(dir, vmPidFile)); err == nil {
		if pid, err := strconv.Atoi(string(data)); err == nil {
			killVM(pid, dir)
		}
	}

	if ids, err := loadIDPool("/etc", idsDir); err == nil {
		_ = cleanupPod(dir, ids)
	}

	data, err := proto.Marshal(&taskAPI.DeleteResponse{ExitStatus: 137, ExitedAt: timestamppb.Now()})
	if err != nil {
		return fmt.Errorf("encode delete response: %w", err)
	}

	_, err = stdout.Write(data)

	return err //nolint:wrapcheck // Writing to containerd's pipe.
}

// killVM kills the VM process pid of the pod directory dir. The pid may belong to another process by now,
// so it checks the command line through a pidfd, which cannot move to a new process, before it signals.
func killVM(pid int, dir string) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd) //nolint:errcheck // Read-only use.

	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !isVMCommand(cmdline, dir) {
		return
	}

	_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}

// isVMCommand reports whether a /proc cmdline is the VM process of the pod directory dir.
func isVMCommand(cmdline []byte, dir string) bool {
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")

	return len(args) == 3 && args[1] == VMArg && args[2] == dir
}

// serve is the shim daemon. containerd passes the listener as fd 3 and reads its logs from the "log" FIFO in the bundle.
func serve(f flags) error {
	logOut := io.Writer(os.Stderr)
	if fifo, err := os.OpenFile("log", os.O_WRONLY, 0); err == nil {
		logOut = fifo
	}

	log := slog.New(slog.NewTextHandler(logOut, nil)).With("runtime", "sbxr", "sandbox", f.id)

	l, err := net.FileListener(os.NewFile(3, "socket"))
	if err != nil {
		return fmt.Errorf("shim listener: %w", err)
	}

	srv, err := ttrpc.NewServer()
	if err != nil {
		return fmt.Errorf("create ttrpc server: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &service{
		log: log, pub: newPublisher(os.Getenv("TTRPC_ADDRESS"), f.namespace, log),
		shutdown: cancel, bootTimeout: defaultBootTimeout, mu: sync.Mutex{}, sandbox: nil, tasks: map[string]*task{},
	}

	taskAPI.RegisterTTRPCTaskService(srv, s)
	sandboxAPI.RegisterTTRPCSandboxService(srv, s)

	go func() {
		<-ctx.Done()

		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()

		_ = srv.Shutdown(sctx)
		_ = l.Close()
	}()

	log.Info("shim serving")

	err = srv.Serve(ctx, l)
	if err != nil && !errors.Is(err, ttrpc.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	if addr, ok := l.Addr().(*net.UnixAddr); ok {
		_ = os.Remove(addr.Name)
	}

	return nil
}
