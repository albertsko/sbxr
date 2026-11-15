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

// MountRoot mounts an overlay at root with the image root as the lower layer and new upper and work directories in dir.
// The caller needs mount privileges, for example inside a user namespace. release unmounts the overlay.
func (img *Image) MountRoot(dir, root string) (release func() error, err error) {
	noop := func() error { return nil }

	upper, work := filepath.Join(dir, "upper"), filepath.Join(dir, "work")
	for _, d := range []string{upper, work, root} {
		err := os.Mkdir(d, 0o755) //nolint:gosec // The upper directory becomes the guest /, which is world-readable.
		if err != nil {
			return noop, fmt.Errorf("create overlay directory: %w", err)
		}
	}

	err = mountOverlay(root, map[string]string{"lowerdir": img.Root, "upperdir": upper, "workdir": work})
	if err != nil {
		return noop, err
	}

	return func() error {
		err := unix.Unmount(root, unix.MNT_DETACH)
		if err != nil {
			return fmt.Errorf("unmount %s: %w", root, err)
		}

		return nil
	}, nil
}

// mountOverlay mounts an overlay at target with the file system context API,
// which takes each option separately, so paths may contain commas.
func mountOverlay(target string, dirs map[string]string) error {
	fsfd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	defer unix.Close(fsfd) //nolint:errcheck // The context is only used to create the mount.

	for _, key := range []string{"lowerdir", "upperdir", "workdir"} {
		err = unix.FsconfigSetString(fsfd, key, dirs[key])
		if err != nil {
			return fmt.Errorf("mount overlay: %s %s: %w", key, dirs[key], err)
		}
	}

	// userxattr keeps overlay metadata in user xattrs, which a user namespace may write.
	err = unix.FsconfigSetFlag(fsfd, "userxattr")
	if err == nil {
		err = unix.FsconfigCreate(fsfd)
	}

	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	mfd, err := unix.Fsmount(fsfd, unix.FSMOUNT_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	defer unix.Close(mfd) //nolint:errcheck // The mount stays attached after the close.

	err = unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("mount overlay at %s: %w", target, err)
	}

	return nil
}
