package preflight

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"golang.org/x/sys/unix"
)

// kvmGetAPIVersion is the KVM_GET_API_VERSION ioctl request.
const kvmGetAPIVersion = 0xAE00

// Hypervisor checks that /dev/kvm opens read-write and reports KVM API version 12.
func Hypervisor(context.Context) Result {
	const id = "hypervisor"

	fd, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Failed(id, "/dev/kvm does not exist", "enable virtualization (KVM) for this machine")
	case errors.Is(err, fs.ErrPermission):
		return Failed(id, "/dev/kvm is not readable and writable", `sudo usermod -aG kvm "$USER" and log in again`)
	case err != nil:
		return Failed(id, fmt.Sprintf("open /dev/kvm: %v", err), "check that KVM works on this machine")
	}

	defer unix.Close(fd)

	version, err := unix.IoctlRetInt(fd, kvmGetAPIVersion)
	if err != nil || version != 12 {
		return Failed(id, fmt.Sprintf("KVM API version %d, error %v; want 12", version, err), "use a Linux kernel with standard KVM")
	}

	return Passed(id, "/dev/kvm is usable")
}
