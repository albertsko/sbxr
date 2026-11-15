//go:build linux

package shim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/api/types"
	"golang.org/x/sys/unix"
)

// idmapBind mounts src at dst through an idmapped bind mount that maps file owners through the user namespace usernsFD.
// The VM process, root in that namespace, then sees the files with their real owners.
// dst must exist and match the type of src.
func idmapBind(src, dst string, usernsFD int, readOnly bool) error {
	tree, err := unix.OpenTree(unix.AT_FDCWD, src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone mount %s: %w", src, err)
	}
	defer unix.Close(tree) //nolint:errcheck // The mount moved, or it goes with the descriptor.

	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_IDMAP, Userns_fd: uint64(usernsFD)} //nolint:gosec // A descriptor is not negative.
	if readOnly {
		attr.Attr_set |= unix.MOUNT_ATTR_RDONLY
	}

	err = unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, attr)
	if err != nil {
		return fmt.Errorf("idmap mount %s: %w", src, err)
	}

	err = unix.MoveMount(tree, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("move mount %s to %s: %w", src, dst, err)
	}

	return nil
}

// shareFile mounts src at dst like idmapBind and creates dst first with the type of src.
// A file system without idmapped mount support gets a plain bind, so the guest sees overflow owners but can still read it.
func shareFile(src, dst string, usernsFD int, readOnly bool) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("share %s: %w", src, err)
	}

	err = os.MkdirAll(filepath.Dir(dst), 0o755) //nolint:gosec // The guest reads the share.
	if err != nil {
		return fmt.Errorf("share %s: %w", src, err)
	}

	if st.IsDir() {
		err = os.Mkdir(dst, 0o755) //nolint:gosec // See above.
	} else {
		err = os.WriteFile(dst, nil, 0o644) //nolint:gosec // See above.
	}

	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("share %s: %w", src, err)
	}

	err = idmapBind(src, dst, usernsFD, readOnly)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EPERM) {
		return err
	}

	flags := uintptr(unix.MS_BIND | unix.MS_REC)

	err = unix.Mount(src, dst, "", flags, "")
	if err == nil && readOnly {
		err = unix.Mount("", dst, "", flags|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	}

	if err != nil {
		return fmt.Errorf("bind %s: %w", src, err)
	}

	return nil
}

// mountRootfs mounts a container's root file system from containerd's mounts at target.
// An overlay keeps its upper layer on the host and gets idmapped lower layers under layers.
// Overlayfs itself cannot be idmapped, but it accepts idmapped lower layers, and copy-up keeps their mapped owners.
func mountRootfs(mounts []*types.Mount, target, layers string, usernsFD, uid int) error {
	if len(mounts) != 1 {
		return fmt.Errorf("root file system has %d mounts, want 1", len(mounts))
	}

	m := mounts[0]

	err := os.MkdirAll(target, 0o755) //nolint:gosec // The guest reads the share.
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}

	switch m.GetType() {
	case bindType:
		return idmapBind(m.GetSource(), target, usernsFD, contains(m.GetOptions(), "ro"))
	case "overlay":
		return mountOverlay(m.GetOptions(), target, layers, usernsFD, uid)
	}

	return fmt.Errorf("root file system type %q is not supported", m.GetType())
}

func mountOverlay(options []string, target, layers string, usernsFD, uid int) error {
	out := make([]string, 0, len(options))

	for _, o := range options {
		key, value, _ := strings.Cut(o, "=")

		switch key {
		case "lowerdir":
			var lowers []string

			for i, dir := range strings.Split(value, ":") {
				dst := filepath.Join(layers, strconv.Itoa(i))

				err := os.MkdirAll(dst, 0o700)
				if err == nil {
					err = idmapBind(dir, dst, usernsFD, true)
				}

				if err != nil {
					return err
				}

				lowers = append(lowers, dst)
			}

			o = "lowerdir=" + strings.Join(lowers, ":")
		case "upperdir":
			// New files take the owners of their lower layer, so the upper directory itself joins the pod's id range.
			err := os.Chown(value, uid, uid)
			if err != nil {
				return fmt.Errorf("chown upper layer: %w", err)
			}
		}

		out = append(out, o)
	}

	err := unix.Mount("overlay", target, "overlay", 0, strings.Join(out, ","))
	if err != nil {
		return fmt.Errorf("mount overlay at %s: %w", target, err)
	}

	return nil
}

// unmountAll detaches every mount at or below dir, deepest first.
func unmountAll(dir string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}

	var targets []string

	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) > 4 && (f[4] == dir || strings.HasPrefix(f[4], dir+"/")) {
			targets = append(targets, f[4])
		}
	}

	var errs []error

	for _, target := range slices.Backward(targets) {
		if err := unix.Unmount(target, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
			errs = append(errs, fmt.Errorf("unmount %s: %w", target, err))
		}
	}

	return errors.Join(errs...)
}

func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}
