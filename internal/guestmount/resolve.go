package guestmount

import (
	"errors"
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

// Resolve returns the guest path p with each symlink resolved inside root, the directory that holds the guest root.
// The first missing component ends the lookup, and the rest of p stays as it is.
func Resolve(root, p string) (string, error) {
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

		target, err := os.Readlink(filepath.Join(root, next))

		switch {
		case errors.Is(err, fs.ErrNotExist):
			return path.Join(append([]string{next}, rest...)...), nil
		case errors.Is(err, syscall.EINVAL):
			cur = next // Not a symlink.
		case err != nil:
			return "", err //nolint:wrapcheck // The caller names the path.
		default:
			links++
			if links > maxSymlinks {
				return "", syscall.ELOOP
			}

			if path.IsAbs(target) {
				cur = "/"
			}

			rest = append(strings.Split(target, "/"), rest...)
		}
	}

	return cur, nil
}
