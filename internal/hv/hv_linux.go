package hv

import (
	"fmt"
	"os"
	"syscall"
)

const (
	kvmDevice = "/dev/kvm"

	// KVM_GET_API_VERSION is _IO(KVMIO, 0x00) with KVMIO 0xAE.
	kvmGetAPIVersion = 0xAE00

	// The KVM API version has been 12 since Linux 2.6.22.
	kvmAPIVersion = 12
)

// Check returns nil when /dev/kvm opens read-write and reports the stable KVM API version.
func Check() error {
	f, err := os.OpenFile(kvmDevice, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s read-write: %w", kvmDevice, err)
	}
	defer func() { _ = f.Close() }()

	version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("%s KVM_GET_API_VERSION: %w", kvmDevice, errno)
	}

	if version != kvmAPIVersion {
		return fmt.Errorf("%s reports KVM API version %d, want %d", kvmDevice, version, kvmAPIVersion)
	}

	return nil
}
