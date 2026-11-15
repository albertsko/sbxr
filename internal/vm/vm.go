// Package vm is the child side of a run: the process that becomes the VM.
//
// The parent writes a Machine into the run directory and starts the same binary with ChildArg.
// The child reads the Machine and hands it to a Hypervisor, which takes over the process.
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// ChildArg is the hidden first argument that selects the child mode.
const ChildArg = "__vm"

// ExitSetup is the exit code for a failure before the workload starts.
const ExitSetup = 125

const machineFile = "machine.json"

// Machine describes one VM.
type Machine struct {
	Root      string `json:"root"`
	CPUs      int    `json:"cpus"`
	MemoryMiB uint32 `json:"memoryMiB"`
}

// Hypervisor starts a VM in the calling process.
// On success, Start never returns: the process exits with the workload's exit code.
type Hypervisor interface {
	Start(m Machine) error
}

// WriteMachine stores m in the run directory dir.
func WriteMachine(dir string, m Machine) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode machine: %w", err)
	}

	err = os.WriteFile(filepath.Join(dir, machineFile), data, 0o600)
	if err != nil {
		return fmt.Errorf("write machine: %w", err)
	}

	return nil
}

// Child runs the child mode with the arguments after ChildArg and returns the exit code.
// It returns only when the VM could not start.
func Child(args []string, h Hypervisor, stderr io.Writer) int {
	runDir := ""
	if len(args) == 1 {
		runDir = args[0]
	}

	watchLifeline(runDir)

	// The VM process shares the runner's process group. Terminal interrupts end the run only through the runner.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)

	err := child(args, h)

	// A failed diagnostic write does not change the exit code.
	_, _ = fmt.Fprintf(stderr, "sbxr: vm: %v\n", err)

	return ExitSetup
}

func child(args []string, h Hypervisor) error {
	if len(args) != 1 {
		return fmt.Errorf("want 1 argument (the run directory), got %d", len(args))
	}

	data, err := os.ReadFile(filepath.Join(args[0], machineFile))
	if err != nil {
		return fmt.Errorf("read machine: %w", err)
	}

	var m Machine

	err = json.Unmarshal(data, &m)
	if err != nil {
		return fmt.Errorf("decode machine: %w", err)
	}

	err = h.Start(m)
	if err == nil {
		return errors.New("the hypervisor returned without running the workload")
	}

	return fmt.Errorf("start: %w", err)
}
