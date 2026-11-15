package hv

import (
	"errors"
	"fmt"
	"syscall"
)

// Check returns nil when Hypervisor.framework is available.
func Check() error {
	support, err := syscall.SysctlUint32("kern.hv_support")
	if err != nil {
		return fmt.Errorf("read sysctl kern.hv_support: %w", err)
	}

	if support != 1 {
		return errors.New("kern.hv_support is 0: Hypervisor.framework is not available")
	}

	return nil
}
