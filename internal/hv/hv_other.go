//go:build !darwin && !linux

package hv

import (
	"fmt"
	"runtime"
)

// Check always fails, because sbxr supports only macOS and Linux hosts.
func Check() error {
	return fmt.Errorf("hardware virtualization is not supported on %s", runtime.GOOS)
}
