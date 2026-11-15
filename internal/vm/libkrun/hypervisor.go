//go:build darwin || linux

package libkrun

import (
	"fmt"
	"math"

	"github.com/albertsko/sbxr/internal/vm"
)

// Hypervisor starts machines with libkrun.
type Hypervisor struct {
	// LibDir is the SBXR_LIB_DIR value, empty for the default library directories.
	LibDir string
}

// Start enters the VM described by m. On success it does not return.
func (h Hypervisor) Start(m vm.Machine) error {
	dir, err := LibDir(h.LibDir)
	if err != nil {
		return err
	}

	if m.CPUs < 1 || m.CPUs > math.MaxUint8 {
		return fmt.Errorf("VM CPU count %d is out of range", m.CPUs)
	}

	return Enter(dir, Config{Root: m.Root, CPUs: uint8(m.CPUs), MemoryMiB: m.MemoryMiB, Net: nil, LogLevel: m.LogLevel})
}
