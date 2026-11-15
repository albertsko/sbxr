package vm

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// unmountUnder detaches every mount below dir, so that removing dir does not reach into a mounted file system.
func unmountUnder(dir string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // The file was only read.

	prefix := filepath.Clean(dir) + "/"

	s := bufio.NewScanner(f)
	for s.Scan() {
		// The fifth field is the mount point, with spaces escaped as \040.
		fields := strings.Fields(s.Text())
		if len(fields) > 4 && strings.HasPrefix(fields[4], prefix) {
			_ = unix.Unmount(fields[4], unix.MNT_DETACH) // A failed unmount leaves only a leftover directory.
		}
	}
}
