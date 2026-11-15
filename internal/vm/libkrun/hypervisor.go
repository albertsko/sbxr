//go:build darwin || linux

package libkrun

import (
	"fmt"
	"math"
	"net"

	"github.com/albertsko/sbxr/internal/guestmount"
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

	var netDev *Net

	if m.Net != nil {
		mac, err := net.ParseMAC(m.Net.MAC)
		if err != nil || len(mac) != len(Net{}.MAC) {
			return fmt.Errorf("VM MAC address %q is not valid", m.Net.MAC)
		}

		netDev = &Net{FD: m.Net.FD, MAC: [6]byte(mac)}
	}

	volumes := make([]Volume, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		volumes = append(volumes, Volume{Tag: v.Tag, Host: v.Host, ReadOnly: v.ReadOnly})
	}

	var helper []byte

	if len(volumes) > 0 {
		helper, err = guestmount.Binary()
		if err != nil {
			return err //nolint:wrapcheck // The error names the helper.
		}
	}

	return Enter(dir, Config{
		Root: m.Root, CPUs: uint8(m.CPUs), MemoryMiB: m.MemoryMiB, Net: netDev, Volumes: volumes, MountHelper: helper, LogLevel: m.LogLevel,
	})
}
