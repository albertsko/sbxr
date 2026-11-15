package guestmount

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/albertsko/sbxr/internal/krunconfig"
)

// maxSymlinks is the Linux limit of symlinks in one path lookup.
const maxSymlinks = 40

// reservedPaths are guest paths that the guest init, the kernel, or sbxr own.
var reservedPaths = []string{"/dev", "/proc", "/sys", Path, "/" + krunconfig.FileName, "/init.krun"}

// Reserved returns the reserved guest path that p is at or under, or "" when p is free for a volume.
func Reserved(p string) string {
	for _, r := range reservedPaths {
		if p == r || strings.HasPrefix(p, r+"/") {
			return r
		}
	}

	return ""
}

// MountPoint walks the guest path p inside root, the directory that holds the guest root, like the kernel resolves it.
// It follows each symlink and creates each missing component as a directory, so `..` and symlinks after it apply to real directories.
// It returns the resolved guest path, which the mount must use. It fails when the result is reserved,
// and it never creates a directory in a reserved path.
func MountPoint(root, p string) (string, error) {
	rest := strings.Split(p, "/")
	cur := "/"
	links := 0

	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]

		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)

			continue
		}

		next := path.Join(cur, c)
		host := filepath.Join(root, next)

		target, err := os.Readlink(host)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			if r := Reserved(next); r != "" {
				return "", reservedError(p, next, r)
			}

			// Workload users other than root must reach the mount point.
			if err := os.Mkdir(host, 0o755); err != nil { //nolint:gosec // See above.
				return "", err //nolint:wrapcheck // The error names the path.
			}

			cur = next
		case errors.Is(err, syscall.EINVAL):
			cur = next // Not a symlink.
		case err != nil:
			return "", err //nolint:wrapcheck // The error names the path.
		default:
			links++
			if links > maxSymlinks {
				return "", fmt.Errorf("%s: %w", p, syscall.ELOOP)
			}

			if path.IsAbs(target) {
				cur = "/"
			}

			rest = append(strings.Split(target, "/"), rest...)
		}
	}

	if r := Reserved(cur); r != "" {
		return "", reservedError(p, cur, r)
	}

	return cur, nil
}

func reservedError(p, resolved, reserved string) error {
	return fmt.Errorf("%s resolves to %s, which is under the reserved path %s", p, resolved, reserved)
}
