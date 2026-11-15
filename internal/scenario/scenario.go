// Package scenario holds behavior scenarios that run against several targets (PRD "Test contract").
//
// A target is a backend such as the fake VM, the real CLI, or a Kubernetes cluster.
// Each target declares its capabilities, and each scenario runs only on targets that have the ones it needs.
// Scenarios assert on stdout, the exit code, and a target-neutral error class, never on the target name.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// Capability is a feature that a target may support.
type Capability string

// Capabilities from the PRD.
const (
	Stdin        Capability = "stdin"
	TTY          Capability = "tty"
	SplitStderr  Capability = "split-stderr"
	SignalExit   Capability = "signal-exit"
	GracefulStop Capability = "graceful-stop"
	Exec         Capability = "exec"
	Net          Capability = "net"
	NetOff       Capability = "net-off"
	Publish      Capability = "publish"
	DNS          Capability = "dns"
	Volumes      Capability = "volumes"
	MemoryLimit  Capability = "memory-limit"
	Probes       Capability = "probes"
)

// ErrorClass is the target-neutral outcome of a run.
type ErrorClass string

// Error classes from the PRD.
const (
	OK            ErrorClass = "ok"
	InvalidSpec   ErrorClass = "invalid-spec"
	Image         ErrorClass = "image"
	Preflight     ErrorClass = "preflight"
	NotFound      ErrorClass = "not-found"
	NotExecutable ErrorClass = "not-executable"
	Interrupted   ErrorClass = "interrupted"
)

// Images that every target provides. Each one also has the files of the test layer (see ServeImages).
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

// Workload is one run request.
type Workload struct {
	Image       string
	Args        []string
	Env         []string
	Workdir     string
	User        string
	Interactive bool
	Stdin       string // Used only when Interactive is set.
	TTY         bool
	// InterruptAfter makes the target interrupt the run once stdout contains this text.
	InterruptAfter string
	// Terminate makes the interrupt a termination (SIGTERM) instead of Ctrl-C (SIGINT).
	Terminate bool
}

// Result is the outcome of one run.
type Result struct {
	Stdout   string
	ExitCode int
	Class    ErrorClass
}

// Target runs workloads on one backend.
type Target interface {
	Capabilities() []Capability
	// Run runs w. An error means that the target itself failed, not the workload.
	Run(ctx context.Context, w Workload) (Result, error)
	// CheckClean reports any VM process, network listener, pod, or run state that remains from this target.
	CheckClean() error
}

// Scenario is one behavior check.
type Scenario struct {
	Name  string
	Needs []Capability
	// Pending skips the scenario with this reason until the build step that it needs exists.
	Pending string
	Check   func(ctx context.Context, t Target) error
}

// timeout is the hard limit for one scenario.
const timeout = 30 * time.Second

// RunAll runs every scenario as a parallel subtest. newTarget returns a fresh target for one subtest.
func RunAll(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()

	for _, s := range All() {
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()

			if s.Pending != "" {
				t.Skip(s.Pending)
			}

			target := newTarget(t)
			if missing := Lacking(target, s); len(missing) > 0 {
				t.Skipf("target lacks %v", missing)
			}

			err := Verify(t.Context(), s, target)
			if err != nil {
				t.Error(err)
			}
		})
	}
}

// Lacking returns the capabilities that s needs and t lacks.
func Lacking(t Target, s Scenario) []Capability {
	var missing []Capability

	for _, c := range s.Needs {
		if !slices.Contains(t.Capabilities(), c) {
			missing = append(missing, c)
		}
	}

	return missing
}

// Verify runs s against t and then checks that t left nothing behind.
func Verify(ctx context.Context, s Scenario, t Target) error {
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
