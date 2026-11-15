package preflight

import (
	"context"

	"golang.org/x/sys/unix"
)

// Hypervisor checks that the kernel reports Hypervisor.framework support.
func Hypervisor(context.Context) Result {
	const id = "hypervisor"

	v, err := unix.SysctlUint32("kern.hv_support")
	if err != nil || v != 1 {
		return Failed(id, "Hypervisor.framework is not available", "use a Mac with Apple silicon and macOS 15 or later")
	}

	return Passed(id, "Hypervisor.framework available")
}
