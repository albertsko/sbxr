package libkrun

import "runtime"

const (
	libkrunName = "libkrun.so.1"
	krunfwName  = "libkrunfw.so.5"
	libPathVar  = "LD_LIBRARY_PATH"
)

func defaultLibDirs() []string {
	dirs := []string{"/home/linuxbrew/.linuxbrew/lib", "/usr/local/lib"}

	switch runtime.GOARCH {
	case "amd64":
		dirs = append(dirs, "/usr/lib/x86_64-linux-gnu")
	case "arm64":
		dirs = append(dirs, "/usr/lib/aarch64-linux-gnu")
	}

	return append(dirs, "/usr/lib64", "/usr/lib")
}

// serialConsole returns the guest name of the first serial port.
func serialConsole() string {
	if runtime.GOARCH == "arm64" {
		return "ttyAMA0"
	}

	return "ttyS0"
}
