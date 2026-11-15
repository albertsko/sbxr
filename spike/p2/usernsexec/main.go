// Command usernsexec runs a command as root of a new user namespace that maps 0..65535 to BASE..BASE+65535.
// Usage: usernsexec BASE command args...
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func main() {
	base, _ := strconv.Atoi(os.Args[1])
	cmd := exec.Command(os.Args[2], os.Args[3:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: base, Size: 65536}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: base, Size: 65536}},
		GidMappingsEnableSetgroups: true,
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
	}

	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "usernsexec:", err)
		os.Exit(1)
	}
}
