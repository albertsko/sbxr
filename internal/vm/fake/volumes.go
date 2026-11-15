package fake

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/vm"
)

// mount is a volume that the fake mount helper mounted.
type mount struct {
	path     string
	root     *os.Root
	readOnly bool
}

// mountHelper plays the guest mount helper: it mounts the volumes, applies the user and working directory, and runs the workload.
func (g *guest) mountHelper(args []string, stdio Stdio) int {
	a, err := guestmount.Decode(append([]string{guestmount.Path}, args...))
	if err != nil {
		sayf(stdio.Err, "sbxr-mount: %v\n", err)

		return vm.ExitSetup
	}

	for _, m := range a.Mounts {
		v, ok := g.volumes[m.Tag]
		if !ok {
			sayf(stdio.Err, "sbxr-mount: mount %s at %s: no such device\n", m.Tag, m.Path)

			return vm.ExitSetup
		}

		root, err := os.OpenRoot(v.Host)
		if err != nil {
			sayf(stdio.Err, "sbxr-mount: mount %s at %s: %v\n", m.Tag, m.Path, err)

			return vm.ExitSetup
		}

		// Later mounts are deeper, so the first match in reverse order is the closest mount.
		g.mounts = append(g.mounts, mount{path: path.Clean(m.Path), root: root, readOnly: m.ReadOnly || v.ReadOnly})
	}

	g.cfg.UID, g.cfg.GID = strconv.FormatUint(uint64(a.UID), 10), strconv.FormatUint(uint64(a.GID), 10)

	if root, name, _ := g.fs(a.WorkingDir); name != "" {
		if info, err := root.Stat(name); err == nil && info.IsDir() {
			g.cwd = path.Clean(a.WorkingDir)
		}
	}

	applet, err := g.resolve(a.Argv[0])
	if err != nil {
		sayf(stdio.Out, "Couldn't execute '%s' inside the vm: %s\n", a.Argv[0], err)

		return exitCode(err)
	}

	return g.run(applet, a.Argv[1:], stdio)
}

// fs returns the file system of the guest path p and the path inside it.
func (g *guest) fs(p string) (root *os.Root, name string, readOnly bool) {
	abs := g.abs(p)

	for _, m := range slices.Backward(g.mounts) {
		if abs == m.path || strings.HasPrefix(abs, m.path+"/") {
			return m.root, rel(strings.TrimPrefix(abs, m.path)), m.readOnly
		}
	}

	return g.root, rel(abs), false
}

// touch creates each file that does not exist.
func (g *guest) touch(args []string, stdio Stdio) int {
	status := 0

	for _, name := range args {
		err := g.create(name)
		if err != nil {
			sayf(stdio.Err, "touch: %s: %s\n", name, strerror(err))

			status = 1
		}
	}

	return status
}

// create creates the file name if it does not exist.
func (g *guest) create(name string) error {
	root, file, readOnly := g.fs(name)
	if readOnly {
		return syscall.EROFS
	}

	f, err := root.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err //nolint:wrapcheck // The applet prints the error.
	}

	return f.Close() //nolint:wrapcheck // The applet prints the error.
}

// strerror returns the libc text of the errno in err.
func strerror(err error) string {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		s := errno.Error()

		return strings.ToUpper(s[:1]) + s[1:]
	}

	if errors.Is(err, fs.ErrNotExist) {
		return "No such file or directory"
	}

	return fmt.Sprint(err)
}

// remove removes the file name.
func (g *guest) remove(name string) error {
	root, file, readOnly := g.fs(name)
	if readOnly {
		return syscall.EROFS
	}

	return root.Remove(file) //nolint:wrapcheck // The applet prints the error.
}
