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

// AttachLifeline makes the child clean up and exit when the parent dies, even by SIGKILL.
// The child inherits the read end of a pipe, and the parent keeps the write end until release.
// When the parent dies, the kernel closes the write end, and the child sees EOF.
// A parent death signal is not used, because it would kill the child before it removes the run directory.
func AttachLifeline(cmd *exec.Cmd) (release func(), err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create lifeline: %w", err)
	}

	cmd.ExtraFiles = append([]*os.File{r}, cmd.ExtraFiles...)

	return func() {
		_ = r.Close() // Closing a pipe end fails only for a closed file.
		_ = w.Close()
	}, nil
}

// watchLifeline removes the run directory and exits the process at EOF on the lifeline.
// The goroutine keeps running after a hypervisor takes over the main thread.
// On Linux, the run root may be an overlay mount, so it is unmounted first.
//
// The fd is close-on-exec from here on.
func watchLifeline(runDir string) {
	syscall.CloseOnExec(lifelineFD)

	f := os.NewFile(lifelineFD, "lifeline")

	go func() {
		_, _ = io.Copy(io.Discard, f) // Any read result means the parent is gone.

		if runDir != "" {
			unmountUnder(runDir)
			_ = os.RemoveAll(runDir)        // Nobody is left to report a failure to.
			_ = os.Remove(runDir + ".lock") // The run lock next to the directory, see run.newRunDir.
		}

		os.Exit(ExitSetup)
	}()
}
