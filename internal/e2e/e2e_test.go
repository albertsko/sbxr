// Package e2e tests the release build of sbxr end to end: on the host, with real VMs, and in a Kubernetes cluster.
//
// The behavior scenarios run against targets, such as the CLI with a real VM or a Kubernetes cluster.
// Each target declares its capabilities, and each scenario runs only on targets that have the ones it needs.
// Scenarios assert on stdout, the exit code, and a target-neutral error class, never on the target name.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(runInBuildDir(m))
}

func runInBuildDir(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sbxr-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer os.RemoveAll(dir)

	buildDir = dir

	return m.Run()
}

// capability is a feature that a target may support.
type capability string

// Capabilities that scenarios can require.
const (
	Stdin        capability = "stdin"
	TTY          capability = "tty"
	SplitStderr  capability = "split-stderr"
	SignalExit   capability = "signal-exit"
	GracefulStop capability = "graceful-stop"
	Exec         capability = "exec"
	Net          capability = "net"
	NetOff       capability = "net-off"
	Publish      capability = "publish"
	DNS          capability = "dns"
	Volumes      capability = "volumes"
	MemoryLimit  capability = "memory-limit"
	Probes       capability = "probes"
	// SpecValidation means that the target rejects invalid run input with class invalid-spec before anything starts.
	SpecValidation capability = "spec-validation"
	// ExactConfig means that the workload sees the image config and the run flags and nothing that the platform adds.
	ExactConfig capability = "exact-config"
	// EgressGuard means that Net and AllowLAN select the CLI egress policy (docs/cli.md, section "Egress policy"), and that the host resolver answers DNS.
	EgressGuard capability = "egress-guard"
)

// errorClass is the target-neutral outcome of a run.
type errorClass string

// Error classes that every target maps its own failures to.
const (
	OK            errorClass = "ok"
	InvalidSpec   errorClass = "invalid-spec"
	Image         errorClass = "image"
	Preflight     errorClass = "preflight"
	NotFound      errorClass = "not-found"
	NotExecutable errorClass = "not-executable"
	Interrupted   errorClass = "interrupted"
)

// Images that every target provides. Each one also has the files of the test layer (see serveImages).
const (
	// Busybox has busybox applets in /bin, PATH=/bin, no entrypoint, and Cmd sh.
	// /etc/passwd is a file without execute permission.
	Busybox = "busybox"
	// Configured is Busybox with Entrypoint [sh -c], Cmd ["echo $A $B; id -u; id -g; pwd"],
	// Env A=image and B=image, WorkingDir /tmp, and User 1000:1000.
	Configured = "configured"
	// NamedUser is Busybox with User nobody.
	NamedUser = "named-user"
	// MissingImage never exists.
	MissingImage = "missing"
)

// workload is one run request.
type workload struct {
	Image       string
	Args        []string
	Env         []string
	Workdir     string
	User        string
	Interactive bool
	Stdin       string // Used only when Interactive is set.
	TTY         bool
	// Memory is the memory limit in MiB, or 0 for the target's default.
	Memory int
	// InterruptAfter makes the target interrupt the run once stdout contains this text.
	InterruptAfter string
	// Terminate makes the interrupt a termination (SIGTERM) instead of Ctrl-C (SIGINT).
	Terminate bool
	// Net gives the workload outbound IPv4, and AllowLAN also LAN addresses.
	Net      bool
	AllowLAN bool
	// Publish lists guest TCP ports that the target publishes on 127.0.0.1.
	Publish []publishPort
	// Published is called once per published port with its host address, while the workload runs.
	Published func(ctx context.Context, guestPort uint16, hostAddr string)
	// Volumes share host directories with the workload.
	Volumes []volume
}

// publishPort publishes the guest TCP port Guest on the host port Host of 127.0.0.1. Host 0 picks a free port.
type publishPort struct {
	Host, Guest uint16
}

// volume is one -v flag.
type volume struct {
	Host, Guest string
	ReadOnly    bool
}

// runResult is the outcome of one run.
type runResult struct {
	Stdout string
	// Stderr helps to explain a failure. Scenarios do not assert on it.
	Stderr   string
	ExitCode int
	Class    errorClass
}

// target runs workloads on one backend.
type target interface {
	Capabilities() []capability
	// Run runs w. An error means that the target itself failed, not the workload.
	Run(ctx context.Context, w workload) (runResult, error)
	// CheckClean reports any VM process, network listener, pod, or run state that remains from this target.
	CheckClean() error
}

// scenario is one behavior check.
type scenario struct {
	Name  string
	Needs []capability
	// Pending skips the scenario with this reason until the build step that it needs exists.
	Pending string
	Check   func(ctx context.Context, t target) error
}

// timeout is the hard limit for one scenario.
const timeout = 30 * time.Second

// runAll runs every scenario as a parallel subtest. newTarget returns a fresh target for one subtest.
func runAll(t *testing.T, newTarget func(t *testing.T) target) {
	t.Helper()

	for _, s := range allScenarios() {
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()

			if s.Pending != "" {
				t.Skip(s.Pending)
			}

			target := newTarget(t)
			if missing := lacking(target, s); len(missing) > 0 {
				t.Skipf("target lacks %v", missing)
			}

			err := verify(t.Context(), s, target)
			if skip, ok := errors.AsType[skipError](err); ok {
				t.Skip(string(skip))
			}

			if err != nil {
				t.Error(err)
			}
		})
	}
}

// lacking returns the capabilities that s needs and t lacks.
func lacking(t target, s scenario) []capability {
	var missing []capability

	for _, c := range s.Needs {
		if !slices.Contains(t.Capabilities(), c) {
			missing = append(missing, c)
		}
	}

	return missing
}

// verify runs s against t and then checks that t left nothing behind.
func verify(ctx context.Context, s scenario, t target) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	checkErr := s.Check(ctx, t)
	if checkErr != nil {
		checkErr = fmt.Errorf("scenario: %w", checkErr)
	}

	cleanErr := t.CheckClean()
	if cleanErr != nil {
		cleanErr = fmt.Errorf("cleanup: %w", cleanErr)
	}

	return errors.Join(checkErr, cleanErr)
}

// skipError makes a scenario skip, for example when the host lacks something that the scenario needs.
type skipError string

func (e skipError) Error() string { return "skip: " + string(e) }

// checkClean reports run state under cacheDir/runs and any process whose command line mentions cacheDir.
// It looks only at this one directory, so parallel tests do not see each other.
func checkClean(cacheDir string) error {
	runs, err := os.ReadDir(cacheDir + "/runs")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read runs directory: %w", err)
	}

	var errs []error

	for _, r := range runs {
		errs = append(errs, fmt.Errorf("run state remains: %s", r.Name()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "pid=,args=").Output()
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}

	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, cacheDir) {
			errs = append(errs, fmt.Errorf("process remains: %s", strings.TrimSpace(line)))
		}
	}

	return errors.Join(errs...)
}

// TestCheckCleanReportsLeftovers checks that checkClean finds run state and a process whose arguments name the cache directory.
func TestCheckCleanReportsLeftovers(t *testing.T) {
	t.Parallel()

	cacheDir := filepath.Join(t.TempDir(), "cache")

	err := os.MkdirAll(filepath.Join(cacheDir, "runs", "r1"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// Like the VM process, the sleep process names the cache directory in its arguments.
	sleep := exec.CommandContext(ctx, "sleep", "30")
	sleep.Args[0] = filepath.Join(cacheDir, "sleep")

	err = sleep.Start()
	if err != nil {
		t.Fatal(err)
	}

	err = checkClean(cacheDir)
	if err == nil || !strings.Contains(err.Error(), "run state remains: r1") || !strings.Contains(err.Error(), "process remains: ") {
		t.Errorf("checkClean() = %v, want the run state and the sleep process", err)
	}

	_ = sleep.Process.Kill()
	_ = sleep.Wait() // The kill ends the process.

	err = os.Remove(filepath.Join(cacheDir, "runs", "r1"))
	if err != nil {
		t.Fatal(err)
	}

	err = checkClean(cacheDir)
	if err != nil {
		t.Errorf("checkClean() after the cleanup = %v, want nil", err)
	}
}

// requireHypervisor skips t when the `hypervisor` doctor check fails, or fails t when SBXR_TEST_REQUIRE=1.
func requireHypervisor(t *testing.T) {
	t.Helper()

	res := preflight.Hypervisor(t.Context())
	if res.Status == preflight.Pass {
		return
	}

	testenv.SkipOrFailf(t, "no hypervisor: %s", res.Detail)
}

// classify maps the CLI exit code and stderr to an error class.
// sbxr failures exit 125 and print a line that starts with "sbxr: <source>".
func classify(code int, stderr string) errorClass {
	sbxrErr, failed := "", false

	for line := range strings.Lines(stderr) {
		if after, ok := strings.CutPrefix(line, "sbxr: "); ok && code == 125 {
			sbxrErr, failed = after, true
		}
	}

	switch {
	case failed && strings.HasPrefix(sbxrErr, "preflight "):
		return Preflight
	case failed && strings.HasPrefix(sbxrErr, "image "):
		return Image
	case failed:
		return InvalidSpec
	case code == 126:
		return NotExecutable
	case code == 127:
		return NotFound
	case code == 130 || code == 143:
		return Interrupted
	default:
		return OK
	}
}
