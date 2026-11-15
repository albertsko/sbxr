//go:build linux

package shim

import (
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
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/albertsko/sbxr/internal/podvm"
	"github.com/albertsko/sbxr/internal/subid"
)

// TestPodDirDoesNotFollowSymlinks plants symlinks where the shim creates files, as a guest with a writable share could.
func TestPodDirDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	writeTestFile(t, filepath.Join(outside, "secret"), "keep")

	d, err := openPodDir(filepath.Join(tmp, "pod"), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	if err := d.mkdir(shareDir); err != nil {
		t.Fatal(err)
	}

	mustSymlink(t, outside, d.join("share/c1"))
	mustSymlink(t, filepath.Join(outside, "secret"), d.join("share/resolv.conf"))

	for name, err := range map[string]error{
		"file through a linked directory": d.writeFile("share/c1/config.json", []byte("x"), fileMode),
		"linked file":                     d.writeFile("share/resolv.conf", []byte("x"), 0o644),
		"mount target":                    targetErr(d.target("share/c1/rootfs", true)),
		"directory":                       d.mkdir("share/c1/mounts"),
	} {
		if !errors.Is(err, unix.ELOOP) {
			t.Errorf("%s: error %v, want ELOOP", name, err)
		}
	}

	if got := readTestFile(t, filepath.Join(outside, "secret")); got != "keep" {
		t.Errorf("outside file = %q, want it unchanged", got)
	}

	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("outside directory has %d entries, want 1", len(entries))
	}

	st, err := os.Stat(d.join(shareDir))
	if err != nil || st.Mode().Perm() != dirMode {
		t.Errorf("share mode %v, %v; want %o", st.Mode(), err, dirMode)
	}
}

func TestDialSocketChecksTheSocket(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	d, err := openPodDir(filepath.Join(tmp, "pod"), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	if err := d.mkdirFor(sockDir, os.Getuid(), 0o700); err != nil {
		t.Fatal(err)
	}

	l := listenUnix(t, d.join(streamSocket))
	other := listenUnix(t, filepath.Join(tmp, "other.sock"))

	conn, err := d.dialSocket(streamSocket, os.Getuid())
	if err != nil {
		t.Fatalf("dial the VM socket: %v", err)
	}

	_ = conn.Close()

	if _, err := d.dialSocket(streamSocket, os.Getuid()+1); err == nil {
		t.Error("dial a socket of another owner: no error")
	}

	mustSymlink(t, other.Addr().String(), d.join(controlSocket))

	if _, err := d.dialSocket(controlSocket, os.Getuid()); !errors.Is(err, unix.ELOOP) {
		t.Errorf("dial through a symlink: %v, want ELOOP", err)
	}

	_ = l.Close()
}

func TestMountsBelow(t *testing.T) {
	t.Parallel()

	const mountinfo = `22 1 0:21 / /run rw - tmpfs tmpfs rw
30 22 0:40 / /run/sbxr/pods/a/share/c1/rootfs rw - overlay overlay rw
31 22 0:41 / /run/sbxr/pods/a/share/c1/mounts/0 rw - tmpfs tmpfs rw
32 22 0:42 / /run/sbxr/pods/ab/share rw - tmpfs tmpfs rw
33 22 0:43 / /run/sbxr/pods/a/share/my\040volume rw - tmpfs tmpfs rw
`

	got := mountsBelow(mountinfo, "/run/sbxr/pods/a")
	want := []string{"/run/sbxr/pods/a/share/c1/rootfs", "/run/sbxr/pods/a/share/c1/mounts/0", "/run/sbxr/pods/a/share/my volume"}

	if !slices.Equal(got, want) {
		t.Errorf("mountsBelow = %q, want %q", got, want)
	}
}

func TestIDPool(t *testing.T) {
	t.Parallel()

	p := idPool{r: subid.Range{Name: subid.Name, First: 1 << 20, Count: 2 * subid.PerPod}, dir: t.TempDir()}

	a, errA := p.allocate("a")
	b, errB := p.allocate("b")
	_, errC := p.allocate("c")

	if errA != nil || errB != nil || a != 1<<20 || b != 1<<20+subid.PerPod || errC == nil {
		t.Fatalf("allocate = %d %v, %d %v, third %v; want two slots and then an error", a, errA, b, errB, errC)
	}

	p.release(1 << 30) // Outside the range: ignored.
	p.release(a)

	if again, err := p.allocate("d"); err != nil || again != a {
		t.Errorf("allocate after release = %d, %v; want %d", again, err, a)
	}
}

func TestDrain(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()

		// A child of the process keeps the output open, so the copy never ends.
		held := make(chan struct{})
		p := &procIO{outputs: []*os.File{w}}
		p.copies.Go(func() { <-held })

		start := time.Now()

		p.drain(2 * time.Second)

		if took := time.Since(start); took != 2*time.Second {
			t.Errorf("drain took %s, want the 2 s timeout", took)
		}

		if _, err := w.WriteString("x"); !errors.Is(err, os.ErrClosed) {
			t.Errorf("output after drain: %v, want it closed", err)
		}

		close(held)

		// Output that ends returns at once.
		var done procIO

		done.copies.Go(func() {})

		start = time.Now()

		done.drain(2 * time.Second)

		if took := time.Since(start); took != 0 {
			t.Errorf("drain of ended output took %s", took)
		}
	})
}

func TestVMCPUs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		quota, period int64
		want          int
	}{
		{0, 0, min(255, max(1, runtime.NumCPU()))},
		{150000, 100000, min(2, runtime.NumCPU())},
		{1000, 100000, 1},
	}

	for _, tt := range tests {
		if got := vmCPUs(podConfig{CPUQuota: tt.quota, CPUPeriod: tt.period}); int(got) != tt.want {
			t.Errorf("vmCPUs(%d/%d) = %d, want %d", tt.quota, tt.period, got, tt.want)
		}
	}
}

// TestBootWatchdog checks PRD "Kubernetes, phase 2": a VM whose guest agent does not answer, or whose VM process fails
// at once (for example on the TAP device), is stopped within the boot timeout, and the sandbox start reports an error.
func TestBootWatchdog(t *testing.T) {
	t.Parallel()

	for name, script := range map[string]string{
		"agent never answers": "sleep 60",
		"VM process fails":    "echo 'sbxr vm: open TAP device sbxr0: operation not permitted' >&2; exit 1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d, err := openPodDir(filepath.Join(t.TempDir(), "pod"), os.Getgid())
			if err != nil {
				t.Fatal(err)
			}
			defer d.close()

			logFile, err := os.Create(d.join(vmLog))
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
			cmd.Stdout, cmd.Stderr = logFile, logFile

			vm := startTestVM(t, cmd)
			_ = logFile.Close()

			s := &service{log: slog.New(slog.DiscardHandler), bootTimeout: 500 * time.Millisecond, tasks: map[string]*task{}}
			sb := &sandbox{id: "pod", dir: d, uid: os.Getuid(), vm: vm, usernsFD: -1}

			start := time.Now()
			_, _, err = s.connectAgent(t.Context(), sb, vm)
			s.stopVM(sb, 0)

			select {
			case <-vm.done:
			default:
				t.Error("the VM process still runs")
			}

			if err == nil || time.Since(start) > 2*time.Second {
				t.Fatalf("connectAgent: %v after %s; want an error within the boot timeout", err, time.Since(start))
			}

			if name == "VM process fails" && !strings.Contains(err.Error(), "open TAP device") {
				t.Errorf("error %q lacks the VM log", err)
			}
		})
	}
}

func TestVMConfigFailsOnTAP(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root creates the TAP device instead")
	}

	_, err := vmConfig(machine{TAP: "sbxrmissing0", MAC: "02:00:00:00:00:01"})
	if err == nil || !strings.Contains(err.Error(), "open TAP device sbxrmissing0") {
		t.Errorf("vmConfig: %v, want a TAP error before libkrun starts", err)
	}
}

// TestExecRollback checks that a failed host side removes the exec process from the guest again.
func TestExecRollback(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	s := &service{log: slog.New(slog.DiscardHandler), tasks: map[string]*task{"c1": {id: "c1", init: newProc("c1", "", &procIO{}), execs: map[string]*proc{}}}}
	s.sandbox = &sandbox{id: "pod", agent: agent, usernsFD: -1}

	_, err := s.Exec(t.Context(), &taskAPI.ExecProcessRequest{ID: "c1", ExecID: "probe", Stdout: filepath.Join(t.TempDir(), "missing-fifo")})
	if err == nil {
		t.Fatal("Exec with a missing FIFO succeeded")
	}

	if got := agent.deleted(); !slices.Equal(got, []string{"c1/probe"}) {
		t.Errorf("guest deletes %q, want c1/probe", got)
	}

	if _, _, _, err := s.find("c1", "probe"); err == nil {
		t.Error("the shim still knows the exec")
	}
}

func TestIsVMCommand(t *testing.T) {
	t.Parallel()

	const dir = "/run/sbxr/pods/a"

	for cmdline, want := range map[string]bool{
		"/opt/sbxr/bin/containerd-shim-sbxr-v1\x00vm\x00/run/sbxr/pods/a\x00": true,
		"/opt/sbxr/bin/containerd-shim-sbxr-v1\x00vm\x00/run/sbxr/pods/b\x00": false,
		"sleep\x0060\x00": false,
	} {
		if got := isVMCommand([]byte(cmdline), dir); got != want {
			t.Errorf("isVMCommand(%q) = %v, want %v", cmdline, got, want)
		}
	}
}

type fakeAgent struct {
	taskAPI.TTRPCTaskService

	mu      sync.Mutex
	deletes []string
}

func (a *fakeAgent) Exec(context.Context, *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (a *fakeAgent) Delete(_ context.Context, req *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.deletes = append(a.deletes, req.GetID()+"/"+req.GetExecID())

	return &taskAPI.DeleteResponse{}, nil
}

func (a *fakeAgent) deleted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return slices.Clone(a.deletes)
}

func startTestVM(t *testing.T, cmd *exec.Cmd) *vmProcess {
	t.Helper()

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	vm := &vmProcess{cmd: cmd, done: make(chan struct{})}

	go func() {
		vm.exit = cmd.Wait()
		close(vm.done)
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()

		<-vm.done
	})

	return vm
}

func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	l, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })

	return l
}

func targetErr(f *os.File, err error) error {
	if f != nil {
		_ = f.Close()
	}

	return err
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// TestConnectIORollsBack checks that a failed stream closes the streams and FIFOs that were already open.
func TestConnectIORollsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stdin, stdout := filepath.Join(dir, "stdin"), filepath.Join(dir, "stdout")

	for _, fifo := range []string{stdin, stdout} {
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var guestEnd *net.UnixConn

	dial := func(h podvm.Header) (*net.UnixConn, error) {
		if h.Kind != podvm.Stdin {
			return nil, errors.New("the guest is gone")
		}

		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("socketpair: %w", err)
		}

		ends := [2]*net.UnixConn{}

		for i, fd := range fds {
			f := os.NewFile(uintptr(fd), "stream")
			c, err := net.FileConn(f)
			_ = f.Close()

			if err != nil {
				return nil, fmt.Errorf("stream: %w", err)
			}

			ends[i], _ = c.(*net.UnixConn)
		}

		guestEnd = ends[1]

		return ends[0], nil
	}

	if _, err := connectIO(dial, "c1", "", stdin, stdout, "", false); err == nil {
		t.Fatal("connectIO with a failing stream succeeded")
	}

	_ = guestEnd.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := guestEnd.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("guest end of the stdin stream: %v, want EOF after the rollback", err)
	}
}
