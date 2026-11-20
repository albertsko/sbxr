package vm

import (
	"fmt"
	"math"
	"net"

	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/libkrun"
)

// start enters the VM described by m with libkrun from libDir. On success it does not return.
func start(libDir string, m Machine) error {
	dir, err := libkrun.LibDir(libDir)
	if err != nil {
		return err //nolint:wrapcheck // The error names the library directories.
	}

	if m.CPUs < 1 || m.CPUs > math.MaxUint8 {
		return fmt.Errorf("VM CPU count %d is out of range", m.CPUs)
	}

	var netDev *libkrun.Net

	if m.Net != nil {
		mac, err := net.ParseMAC(m.Net.MAC)
		if err != nil || len(mac) != len(libkrun.Net{}.MAC) {
			return fmt.Errorf("VM MAC address %q is not valid", m.Net.MAC)
		}

		netDev = &libkrun.Net{FD: m.Net.FD, MAC: [6]byte(mac)}
	}

	volumes := make([]libkrun.Volume, 0, len(m.Volumes))
	for _, v := range m.Volumes {
		volumes = append(volumes, libkrun.Volume{Tag: v.Tag, Host: v.Host, ReadOnly: v.ReadOnly})
	}

	var helper []byte

	if len(volumes) > 0 {
		helper, err = guestmount.Binary()
		if err != nil {
			return err //nolint:wrapcheck // The error names the helper.
		}
	}

	return libkrun.Enter(dir, libkrun.Config{ //nolint:wrapcheck // Enter names the failing call.
		Root: m.Root, CPUs: uint8(m.CPUs), MemoryMiB: m.MemoryMiB, Net: netDev, Volumes: volumes,
		MountHelper: helper, MountHelperPath: guestmount.Path, LogLevel: m.LogLevel,
	})
}
