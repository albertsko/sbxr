//go:build vm && (darwin || linux)

package libkrun

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/hv"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/testenv"
)

// The test binary re-executes itself as the VM process when these variables are set.
const (
	vmRootEnv   = "SBXR_TEST_VM_ROOT"
	vmLibDirEnv = "SBXR_TEST_VM_LIB_DIR"
)

func TestMain(m *testing.M) {
	if root := os.Getenv(vmRootEnv); root != "" {
		err := Enter(os.Getenv(vmLibDirEnv), Config{Root: root, CPUs: 1, MemoryMiB: 256})
		fmt.Fprintln(os.Stderr, "enter:", err)
		os.Exit(125)
	}

	os.Exit(m.Run())
}

func TestVM(t *testing.T) {
	t.Parallel()

	libDir := requireVM(t)
	guest := buildGuest(t)

	t.Run("stdout", func(t *testing.T) {
		t.Parallel()

		out, code := runVM(t, libDir, guest, nil, "echo", "hello", "from", "guest")
		if code != 0 || out != "hello from guest\n" {
			t.Errorf("exit code %d, stdout %q, want 0 and %q", code, out, "hello from guest\n")
		}
	})

	t.Run("exit code", func(t *testing.T) {
		t.Parallel()

		if _, code := runVM(t, libDir, guest, nil, "exit", "3"); code != 3 {
			t.Errorf("exit code %d, want 3", code)
		}
	})

	t.Run("no network without a device", func(t *testing.T) {
		t.Parallel()

		var accepted atomic.Int32

		hostAddr := listen(t, func(conn net.Conn) {
			accepted.Add(1)
			conn.Close()
		})
		guestPort := freePort(t)

		var reached []string

		onLine := func(line string) {
			if line != "listening" {
				return
			}

			// The guest listener must not appear on the host, which TSI would do.
			for _, host := range []string{"127.0.0.1", localIPv4(t)} {
				addr := net.JoinHostPort(host, guestPort)
				dialer := net.Dialer{Timeout: time.Second}

				conn, err := dialer.DialContext(t.Context(), "tcp4", addr)
				if err == nil {
					conn.Close()

					reached = append(reached, addr)
				}
			}
		}

		out, code := runVM(t, libDir, guest, onLine, "net", "0.0.0.0:"+guestPort, hostAddr)
		// The TCP dials catch TSI. The vsock dial catches a vsock port that libkrun maps to the host.
		if want := "listening\ndial failed " + hostAddr + "\nvsock dial failed\n"; code != 0 || out != want {
			t.Errorf("exit code %d, stdout %q, want 0 and %q", code, out, want)
		}

		if len(reached) > 0 {
			t.Errorf("host reached the guest listener at %s", strings.Join(reached, ", "))
		}

		if n := accepted.Load(); n > 0 {
			t.Errorf("host listener accepted %d connections from the guest", n)
		}
	})
}

// requireVM skips the test without a usable VM setup, or fails it when SBXR_TEST_REQUIRE=1.
func requireVM(t *testing.T) string {
	t.Helper()

	if err := hv.Check(); err != nil {
		testenv.SkipOrFailf(t, "no hypervisor: %v", err)
	}

	libDir, err := LibDir(os.Getenv("SBXR_LIB_DIR"))
	if err != nil {
		testenv.SkipOrFailf(t, "no libkrun: %v", err)
	}

	if runtime.GOOS == "darwin" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}

		if err := hv.CheckEntitlement(self); err != nil {
			testenv.SkipOrFailf(t, "%v; run go test with -exec scripts/gotest-exec.sh", err)
		}
	}

	if err := CheckLibkrun(libDir); err != nil {
		t.Fatal(err)
	}

	if _, err := CheckLibkrunfw(libDir); err != nil {
		t.Fatal(err)
	}

	return libDir
}

// buildGuest builds the static Linux workload for the host architecture.
func buildGuest(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "guest")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", out, "./testdata/guest")

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)

	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build guest: %v: %s", err, b)
	}

	return out
}

// runVM runs the guest with args in a new root and a new VM process.
// It calls onLine for each stdout line and returns the stdout and the exit code.
func runVM(t *testing.T, libDir, guest string, onLine func(string), args ...string) (string, int) {
	t.Helper()

	cfg := krunconfig.Config{UID: 0, GID: 0, WorkingDir: "/", Env: []string{"PATH=/"}, Args: append([]string{"/guest"}, args...)}

	return runConfig(t, libDir, newRoot(t, guest, cfg), onLine)
}

// runConfig boots root in a new VM process.
func runConfig(t *testing.T, libDir, root string, onLine func(string)) (string, int) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = slices.Concat(os.Environ(), Env(libDir), []string{vmRootEnv + "=" + root, vmLibDirEnv + "=" + libDir})
	cmd.WaitDelay = 5 * time.Second

	var stderr strings.Builder

	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder

	lines := bufio.NewScanner(stdout)
	lines.Buffer(nil, 1<<20)

	for lines.Scan() {
		out.WriteString(lines.Text() + "\n")

		if onLine != nil {
			onLine(lines.Text())
		}
	}

	err = cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.Exited() {
		if stderr.Len() > 0 {
			t.Logf("VM stderr: %s", stderr.String())
		}

		return out.String(), exitErr.ExitCode()
	}

	if err != nil {
		t.Fatalf("VM process: %v: %s", err, stderr.String())
	}

	return out.String(), 0
}

// newRoot writes a guest root with the workload at /guest, the working directory of cfg, and cfg as /.krun_config.json.
func newRoot(t *testing.T, guest string, cfg krunconfig.Config) string {
	t.Helper()

	dir := t.TempDir()

	bin, err := os.ReadFile(guest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "guest"), bin, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, cfg.WorkingDir), 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := krunconfig.Write(root, cfg); err != nil {
		t.Fatal(err)
	}

	return dir
}

// listen starts a TCP listener on 127.0.0.1 and returns its address.
func listen(t *testing.T, handle func(net.Conn)) string {
	t.Helper()

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			handle(conn)
		}
	}()

	return ln.Addr().String()
}

func freePort(t *testing.T) string {
	t.Helper()

	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// localIPv4 returns a non-loopback address of the host, or 127.0.0.1 when it has none.
func localIPv4(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}

	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			return ip.IP.String()
		}
	}

	return "127.0.0.1"
}
