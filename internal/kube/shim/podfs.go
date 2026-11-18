//go:build linux

package shim

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// podDir is a pod directory on the host. Root owns every directory and file that the shim creates in it, and the pod's
// group may only read them, so neither the guest (through the share) nor the VM process can plant a symlink there.
// The shim still resolves every path below it without following symlinks or crossing mounts.
type podDir struct {
	path string
	fd   int
	// owner owns what the shim creates: root, or the test user in tests.
	owner int
	// gid is the group of the pod's VM process. It may read and traverse what the shim creates, and nothing else may.
	gid int
}

// Modes of what the shim creates in a pod directory.
const (
	dirMode  = 0o750
	fileMode = 0o640
)

const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV

// openPodDir creates the pod directory path, owned by root and readable by gid only, and opens it.
// Its parent must exist and let other users only pass through, so a pod cannot list or read another pod's directory.
func openPodDir(path string, gid int) (*podDir, error) {
	err := unix.Mkdir(path, dirMode)
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	d := &podDir{path: path, fd: fd, owner: os.Geteuid(), gid: gid}

	err = d.own(fd, d.owner, dirMode)
	if err != nil {
		_ = unix.Close(fd)

		return nil, err
	}

	return d, nil
}

func (d *podDir) close() { _ = unix.Close(d.fd) }

// join returns the host path of rel, for logs, mount options, and other processes.
func (d *podDir) join(rel string) string { return filepath.Join(d.path, rel) }

// open opens rel below the pod directory. A symlink or a mount anywhere on the way fails with ELOOP or EXDEV.
func (d *podDir) open(rel string, flags int, mode uint32) (int, error) {
	how := &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(mode), Resolve: resolveFlags} //nolint:gosec // Open flags are not negative.

	for {
		fd, err := unix.Openat2(d.fd, rel, how)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}

		if err != nil {
			return -1, fmt.Errorf("open %s: %w", d.join(rel), err)
		}

		return fd, nil
	}
}

// own gives fd to uid and the pod group with mode.
func (d *podDir) own(fd, uid int, mode uint32) error {
	err := unix.Fchown(fd, uid, d.gid)
	if err == nil {
		err = unix.Fchmod(fd, mode)
	}

	if err != nil {
		return fmt.Errorf("set owner of %s: %w", d.path, err)
	}

	return nil
}

// mkdir creates the directory rel and every missing parent, owned by root.
func (d *podDir) mkdir(rel string) error { return d.mkdirFor(rel, d.owner, dirMode) }

// mkdirFor creates the directory rel, owned by uid with mode, and every missing parent, owned by root.
func (d *podDir) mkdirFor(rel string, uid int, mode uint32) error {
	parts := strings.Split(filepath.Clean(rel), "/")

	for i := range parts {
		parent, name := filepath.Join(append([]string{"."}, parts[:i]...)...), parts[i]

		pfd, err := d.open(parent, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}

		err = unix.Mkdirat(pfd, name, dirMode)
		_ = unix.Close(pfd)

		if err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create %s: %w", d.join(rel), err)
		}

		owner, perm := d.owner, uint32(dirMode)
		if i == len(parts)-1 {
			owner, perm = uid, mode
		}

		fd, err := d.open(filepath.Join(parent, name), unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}

		err = d.own(fd, owner, perm)
		_ = unix.Close(fd)

		if err != nil {
			return err
		}
	}

	return nil
}

// writeFile writes the root-owned file rel with mode.
func (d *podDir) writeFile(rel string, data []byte, mode uint32) error {
	fd, err := d.open(rel, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, mode)
	if err != nil {
		return err
	}

	f := os.NewFile(uintptr(fd), d.join(rel))
	defer f.Close() //nolint:errcheck // Closed after the write below.

	err = d.own(fd, d.owner, mode)
	if err == nil {
		_, err = f.Write(data)
	}

	if err != nil {
		return fmt.Errorf("write %s: %w", d.join(rel), err)
	}

	return f.Close() //nolint:wrapcheck // os.File errors name the file.
}

// target creates rel as a directory or an empty file for a mount, and returns it opened with O_PATH.
// Mount it through procPath, so that nothing resolves the path again.
func (d *podDir) target(rel string, dir bool) (*os.File, error) {
	var err error

	if dir {
		err = d.mkdir(rel)
	} else {
		err = d.mkdir(filepath.Dir(rel))
		if err == nil {
			err = d.writeFile(rel, nil, fileMode)
		}
	}

	if err != nil {
		return nil, err
	}

	fd, err := d.open(rel, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), d.join(rel)), nil
}

// procPath is the path of an open file that mount(2) and connect(2) resolve to the file itself.
func procPath(f *os.File) string { return "/proc/self/fd/" + strconv.Itoa(int(f.Fd())) }

// dialSocket connects to the Unix socket rel, which the VM process created and must own.
// The socket is opened first without following symlinks, so the VM process cannot point the shim at another socket.
func (d *podDir) dialSocket(rel string, uid int) (*net.UnixConn, error) {
	fd, err := d.open(rel, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}

	f := os.NewFile(uintptr(fd), d.join(rel))
	defer f.Close() //nolint:errcheck // Only needed for the connect.

	var st unix.Stat_t

	err = unix.Fstat(fd, &st)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", d.join(rel), err)
	}

	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || int(st.Uid) != uid {
		return nil, fmt.Errorf("%s is not a socket of the VM process", d.join(rel))
	}

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: procPath(f), Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", d.join(rel), err)
	}

	return conn, nil
}
