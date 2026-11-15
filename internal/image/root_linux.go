package image

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// CloneRoot creates dst as a private copy of the image root, so a run can change it without changing the cache.
// On Linux it copies the files and keeps hardlinks and OverrideStatXattr.
// Runs that unpack with OwnershipChown mount an overlay inside the user namespace instead.
func (img *Image) CloneRoot(dst string) error {
	err := filepath.WalkDir(img.Root, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(img.Root, src)
		if err != nil {
			return err //nolint:wrapcheck // CloneRoot wraps the error.
		}

		return copyEntry(src, filepath.Join(dst, rel), d)
	})
	if err != nil {
		return fmt.Errorf("copy image root to %s: %w", dst, err)
	}

	return img.relink(dst)
}

func copyEntry(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}

	switch {
	case d.IsDir():
		err = os.Mkdir(dst, 0o700)
	case d.Type()&fs.ModeSymlink != 0:
		var target string

		target, err = os.Readlink(src)
		if err == nil {
			err = os.Symlink(target, dst)
		}

		return err //nolint:wrapcheck // CloneRoot wraps the error.
	case d.Type().IsRegular():
		err = copyFile(src, dst)
	default:
		return nil
	}

	if err != nil {
		return err
	}

	err = os.Chmod(dst, info.Mode().Perm())
	if err != nil {
		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}

	return copyOverrideStat(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}

	_, err = io.Copy(out, in)
	if err != nil {
		_ = out.Close()

		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}

	return out.Close() //nolint:wrapcheck // CloneRoot wraps the error.
}

func copyOverrideStat(src, dst string) error {
	buf := make([]byte, 64)

	n, err := unix.Lgetxattr(src, OverrideStatXattr, buf)
	if errors.Is(err, unix.ENODATA) {
		return nil
	}

	if err != nil {
		return err //nolint:wrapcheck // CloneRoot wraps the error.
	}

	return unix.Lsetxattr(dst, OverrideStatXattr, buf[:n], 0) //nolint:wrapcheck // CloneRoot wraps the error.
}
