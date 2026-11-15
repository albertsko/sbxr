package vm

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
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
		// The fifth field is the mount point.
		fields := strings.Fields(s.Text())
		if len(fields) < 5 {
			continue
		}

		if mnt := unescapeMountPath(fields[4]); strings.HasPrefix(mnt, prefix) {
			_ = unix.Unmount(mnt, unix.MNT_DETACH) // A failed unmount leaves only a leftover directory.
		}
	}
}

// unescapeMountPath decodes the octal escapes, such as \040 for a space, that mountinfo uses for whitespace and backslashes.
func unescapeMountPath(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) && isOctal(s[i+1:i+4]) {
			n, _ := strconv.ParseUint(s[i+1:i+4], 8, 8)
			b.WriteByte(byte(n))

			i += 3

			continue
		}

		b.WriteByte(s[i])
	}

	return b.String()
}

func isOctal(s string) bool {
	return len(s) == 3 && strings.Trim(s, "01234567") == ""
}
