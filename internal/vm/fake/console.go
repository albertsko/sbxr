package fake

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// console emulates the guest kernel's terminal when the VM process's stdin is a terminal.
// Like libkrun, it passes every key to the guest: it turns off signal keys and line editing on the host terminal.
// Like the guest kernel, it ends the workload with exit code 130 on Ctrl-C.
// It returns the guest stdin.
func console(f *os.File) io.Reader {
	t, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	if err != nil {
		return f
	}

	t.Lflag &^= unix.ISIG | unix.ICANON
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0

	err = unix.IoctlSetTermios(int(f.Fd()), setTermios, t)
	if err != nil {
		return f
	}

	pr, pw := io.Pipe()

	go func() {
		buf := make([]byte, 1)

		for {
			_, err := f.Read(buf)
			if err != nil {
				_ = pw.CloseWithError(err)

				return
			}

			if buf[0] == 0x03 {
				os.Exit(130)
			}

			_, _ = pw.Write(buf)
		}
	}()

	return pr
}
