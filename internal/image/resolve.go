package image

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// maxSymlinks matches the Linux limit on symlinks followed in one path lookup.
const maxSymlinks = 40

var errSymlinkLoop = errors.New("too many levels of symbolic links")

// resolveInRoot resolves symlinks in name the way the guest sees them: an absolute target starts at the root, and ".." stops at the root.
// The last component is followed only when followLast is true.
// The result is a clean relative path whose parent directories are not symlinks, so os.Root can use it.
func resolveInRoot(root *os.Root, name string, followLast bool) (string, error) {
	resolved := "."
	rest := strings.Split(name, "/")
	hops := 0

	for len(rest) > 0 {
		part := rest[0]
		rest = rest[1:]

		switch part {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)

			continue
		}

		next := path.Join(resolved, part)
		if len(rest) == 0 && !followLast {
			return next, nil
		}

		fi, err := root.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			resolved = next

			continue
		}

		if err != nil {
			return "", err //nolint:wrapcheck // Callers add the path.
		}

		hops++
		if hops > maxSymlinks {
			return "", fmt.Errorf("resolve %q: %w", name, errSymlinkLoop)
		}

		target, err := root.Readlink(next)
		if err != nil {
			return "", err //nolint:wrapcheck // Callers add the path.
		}

		if path.IsAbs(target) {
			resolved = "."
		}

		rest = append(strings.Split(target, "/"), rest...)
	}

	return resolved, nil
}
