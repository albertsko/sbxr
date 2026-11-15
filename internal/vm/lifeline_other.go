//go:build !linux

package vm

import "os/exec"

// setParentDeathSignal does nothing: only Linux has a parent death signal.
func setParentDeathSignal(*exec.Cmd) {}
