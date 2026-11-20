// Package vm is the child side of a run: the process that becomes the VM.
//
// The parent writes a Machine into the run directory and starts the same binary with ChildArg.
// The child reads the Machine and starts it with libkrun, which takes over the process.
// The package builds only on darwin and linux, because libkrun does.
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

	"github.com/albertsko/sbxr/internal/libkrun"
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
	// LogLevel is the hypervisor log level, from 0 (off) to 5 (trace).
	LogLevel uint32 `json:"logLevel"`
	// Net, when set, attaches the VM to the network of the run. Without it the VM has no network device.
	Net *Net `json:"net,omitempty"`
	// Volumes are host directories that the VM gets as virtio-fs devices.
	Volumes []Volume `json:"volumes,omitempty"`
}

// Volume is a host directory shared with the guest. The guest mounts it by its tag.
type Volume struct {
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	ReadOnly bool   `json:"readOnly"`
}

// NetFD is the VM process fd of the network socket, the last entry of exec.Cmd.ExtraFiles after the lifeline, the cache hold, and the run lock.
const NetFD = 6

// Net is the network device of a VM: a datagram socket that carries one Ethernet frame per message.
type Net struct {
	// FD is the socket fd in the VM process.
	FD int `json:"fd"`
	// MAC is the guest MAC address, for example 5a:94:ef:e4:0c:ee.
	MAC string `json:"mac"`
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

// Env returns the environment entries that let the VM process load libkrun from libDir, the SBXR_LIB_DIR value.
// It returns nil without a library directory, because the libkrun check then fails before any VM starts.
func Env(libDir string) []string {
	dir, err := libkrun.LibDir(libDir)
	if err != nil {
		return nil
	}

	return libkrun.Env(dir)
}

// Child runs the child mode with the arguments after ChildArg and returns the exit code.
// libDir is the SBXR_LIB_DIR value, empty for the default library directories.
// Child returns only when the VM could not start. Otherwise the process exits with the workload's exit code.
func Child(args []string, libDir string, stderr io.Writer) int {
	runDir := ""
	if len(args) == 1 {
		runDir = args[0]
	}

	watchLifeline(runDir)

	// The VM process shares the runner's process group. Terminal interrupts and a closed terminal end the run only through the runner,
	// which then stops the VM and removes the run state. If the runner is gone, the lifeline cleans up.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)

	err := child(args, libDir)

	// A failed diagnostic write does not change the exit code.
	_, _ = fmt.Fprintf(stderr, "sbxr: vm: %v\n", err)

	return ExitSetup
}

func child(args []string, libDir string) error {
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

	err = start(libDir, m)
	if err == nil {
		return errors.New("the hypervisor returned without running the workload")
	}

	return fmt.Errorf("start: %w", err)
}
