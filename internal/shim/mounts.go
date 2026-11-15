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

// bindMount mounts a clone of src on dst, an O_PATH target from podDir.target. With usernsFD >= 0 the mount maps file
// owners through that user namespace, so the VM process, root in it, sees the files with their real owners.
func bindMount(src string, dst *os.File, usernsFD int, readOnly bool) error {
	tree, err := unix.OpenTree(unix.AT_FDCWD, src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone mount %s: %w", src, err)
	}
	defer unix.Close(tree) //nolint:errcheck // The mount moved, or it goes with the descriptor.

	var attr unix.MountAttr

	if usernsFD >= 0 {
		attr.Attr_set, attr.Userns_fd = unix.MOUNT_ATTR_IDMAP, uint64(usernsFD)
	}

	if readOnly {
		attr.Attr_set |= unix.MOUNT_ATTR_RDONLY
	}

	if attr.Attr_set != 0 {
		err = unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &attr)
		if err != nil {
			return fmt.Errorf("set attributes of mount %s: %w", src, err)
		}
	}

	err = unix.MoveMount(tree, "", int(dst.Fd()), "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
	if err != nil {
		return fmt.Errorf("move mount %s to %s: %w", src, dst.Name(), err)
	}

	return nil
}

// shareFile mounts the volume src at rel in the pod directory, idmapped through usernsFD.
// A file system without idmapped mount support gets a plain bind, so the guest sees overflow owners but can still read it.
func shareFile(src string, d *podDir, rel string, usernsFD int, readOnly bool) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("share %s: %w", src, err)
	}

	dst, err := d.target(rel, st.IsDir())
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck // O_PATH descriptor.

	err = bindMount(src, dst, usernsFD, readOnly)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) {
		err = bindMount(src, dst, -1, readOnly)
	}

	return err
}

// mountRootfs mounts a container's root file system from containerd's mounts at rel in the pod directory.
// An overlay keeps its upper layer on the host and gets idmapped lower layers under layers.
// Overlayfs itself cannot be idmapped, but it accepts idmapped lower layers, and copy-up keeps their mapped owners.
func mountRootfs(mounts []*types.Mount, d *podDir, rel, layers string, usernsFD, uid int) error {
	if len(mounts) != 1 {
		return fmt.Errorf("root file system has %d mounts, want 1", len(mounts))
	}

	m := mounts[0]

	dst, err := d.target(rel, true)
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck // O_PATH descriptor.

	switch m.GetType() {
	case bindType:
		return bindMount(m.GetSource(), dst, usernsFD, slices.Contains(m.GetOptions(), "ro"))
	case "overlay":
		return mountOverlay(m.GetOptions(), dst, d, layers, usernsFD, uid)
	}

	return fmt.Errorf("root file system type %q is not supported", m.GetType())
}

func mountOverlay(options []string, dst *os.File, d *podDir, layers string, usernsFD, uid int) error {
	out := make([]string, 0, len(options))

	for _, o := range options {
		key, value, _ := strings.Cut(o, "=")

		switch key {
		case "lowerdir":
			var lowers []string

			for i, dir := range strings.Split(value, ":") {
				rel := filepath.Join(layers, strconv.Itoa(i))

				lower, err := d.target(rel, true)
				if err != nil {
					return err
				}

				err = bindMount(dir, lower, usernsFD, true)
				_ = lower.Close()

				if err != nil {
					return err
				}

				lowers = append(lowers, d.join(rel))
			}

			o = "lowerdir=" + strings.Join(lowers, ":")
		case "upperdir":
			// New files take the owners of their lower layer, so the upper directory itself joins the pod's id range.
			err := unix.Fchownat(unix.AT_FDCWD, value, uid, uid, unix.AT_SYMLINK_NOFOLLOW)
			if err != nil {
				return fmt.Errorf("chown upper layer: %w", err)
			}
		}

		out = append(out, o)
	}

	err := unix.Mount("overlay", procPath(dst), "overlay", 0, strings.Join(out, ","))
	if err != nil {
		return fmt.Errorf("mount overlay at %s: %w", dst.Name(), err)
	}

	return nil
}

// unmountAll detaches every mount at or below dir, deepest first, and fails when any remains.
func unmountAll(dir string) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}

	for _, target := range slices.Backward(mountsBelow(string(data), dir)) {
		err := unix.Unmount(target, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("unmount %s: %w", target, err)
		}
	}

	data, err = os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}

	if left := mountsBelow(string(data), dir); len(left) > 0 {
		return fmt.Errorf("mounts remain below %s: %s", dir, strings.Join(left, ", "))
	}

	return nil
}

// removeTree unmounts dir and then removes it. It removes nothing while a mount, such as a volume, remains.
func removeTree(dir string) error {
	err := unmountAll(dir)
	if err == nil {
		err = os.RemoveAll(dir)
	}

	return err //nolint:wrapcheck // Both name the path.
}

// mountsBelow returns the mount points at or below dir in a mountinfo text, in mount order.
func mountsBelow(mountinfo, dir string) []string {
	var targets []string

	for line := range strings.Lines(mountinfo) {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}

		target := unescapeMountinfo(f[4])
		if target == dir || strings.HasPrefix(target, dir+"/") {
			targets = append(targets, target)
		}
	}

	return targets
}

// unescapeMountinfo decodes the octal escapes (\040 for a space) of a mountinfo path.
func unescapeMountinfo(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))

				i += 3

				continue
			}
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
