//go:build !linux

package vm

// unmountUnder does nothing: run roots are mounts only on Linux.
func unmountUnder(string) {}
