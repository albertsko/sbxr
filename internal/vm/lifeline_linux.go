package vm

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal also kills the child when the parent thread that started it exits.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
