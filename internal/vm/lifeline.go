package vm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// lifelineFD is the child fd of the lifeline pipe, the first entry of exec.Cmd.ExtraFiles.
const lifelineFD = 3

// AttachLifeline makes the child exit when the parent dies, even by SIGKILL.
// The child inherits the read end of a pipe, and the parent keeps the write end until release.
// When the parent dies, the kernel closes the write end, and the child sees EOF.
func AttachLifeline(cmd *exec.Cmd) (release func(), err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create lifeline: %w", err)
	}

	cmd.ExtraFiles = append([]*os.File{r}, cmd.ExtraFiles...)
	setParentDeathSignal(cmd)

	return func() {
		_ = r.Close() // Closing a pipe end fails only for a closed file.
		_ = w.Close()
	}, nil
}

// watchLifeline exits the process at EOF on the lifeline.
// The goroutine keeps running after a hypervisor takes over the main thread.
func watchLifeline() {
	syscall.CloseOnExec(lifelineFD)

	f := os.NewFile(lifelineFD, "lifeline")

	go func() {
		_, _ = io.Copy(io.Discard, f) // Any read result means the parent is gone.

		os.Exit(ExitSetup)
	}()
}
