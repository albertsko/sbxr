//go:build linux

// Command sbxr-mount is the guest mount helper for -v (see internal/guestmount).
// The libkrun init runs it as root. It mounts the volumes, then sets the user, groups, and working directory
// and runs the workload exactly like the libkrun 1.19.8 init, including its messages and exit codes.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/guestmount"
)

// exitSetup is the exit code of the init for a failure before the workload runs.
const exitSetup = 125

func main() {
	a, err := guestmount.Decode(os.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbxr-mount: %v\n", err)
		os.Exit(exitSetup)
	}

	for _, m := range a.Mounts {
		if err := mount(m); err != nil {
			fmt.Fprintf(os.Stderr, "sbxr-mount: mount %s at %s: %v\n", m.Tag, m.Path, err)
			os.Exit(exitSetup)
		}
	}

	// The init changed to the working directory before the volumes existed. Like the init, the helper ignores a failure.
	_ = syscall.Chdir(a.WorkingDir)

	// The syscall package applies these to every thread. The init drops the supplementary groups with the group.
	if syscall.Setgroups(nil) != nil || syscall.Setgid(int(a.GID)) != nil {
		fmt.Println("Can't set GID")
		os.Exit(exitSetup)
	}

	if syscall.Setuid(int(a.UID)) != nil {
		fmt.Println("Can't set UID")
		os.Exit(exitSetup)
	}

	err = execvp(a.Argv)

	fmt.Printf("Couldn't execute '%s' inside the vm: %s\n", a.Argv[0], strerror(err))

	os.Exit(exitCode(err))
}

// exitCode is the init exit code for a failed exec: 127 for a missing file, else 126.
func exitCode(err error) int {
	if errors.Is(err, unix.ENOENT) {
		return 127
	}

	return 126
}

// mount mounts m on its guest path with symlinks resolved, so that an image symlink cannot put a volume over a reserved path.
// Earlier volumes are mounted already, so symlinks inside them count too.
func mount(m guestmount.Mount) error {
	// The same walk resolves, checks, and creates the mount point, so nothing can disagree with the check.
	target, err := guestmount.MountPoint("/", m.Path)
	if err != nil {
		return err //nolint:wrapcheck // The caller names the mount.
	}

	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}

	return unix.Mount(m.Tag, target, "virtiofs", flags, "") //nolint:wrapcheck // The caller names the mount.
}

// execvp runs argv like glibc execvp: through PATH for a bare name, a file in an unknown format through /bin/sh,
// and EACCES only when no directory had the file. An empty name is ENOENT, like in glibc.
func execvp(argv []string) error {
	name := argv[0]
	if name == "" {
		return unix.ENOENT
	}

	if strings.Contains(name, "/") {
		return execFile(name, argv)
	}

	path, ok := os.LookupEnv("PATH")
	if !ok {
		path = "/bin:/usr/bin"
	}

	denied := false

	for dir := range strings.SplitSeq(path, ":") {
		if dir == "" {
			dir = "."
		}

		err := execFile(dir+"/"+name, argv)

		switch {
		case errors.Is(err, unix.EACCES):
			denied = true
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ESTALE),
			errors.Is(err, unix.ENODEV), errors.Is(err, unix.ETIMEDOUT):
		default:
			return err
		}
	}

	if denied {
		return unix.EACCES
	}

	return unix.ENOENT
}

func execFile(file string, argv []string) error {
	err := unix.Exec(file, argv, os.Environ())
	if errors.Is(err, unix.ENOEXEC) {
		err = unix.Exec("/bin/sh", append([]string{"/bin/sh", file}, argv[1:]...), os.Environ())
	}

	return err //nolint:wrapcheck // The errno decides the exit code.
}

// strerror returns the glibc text of err, which matches the Go text with a capital first letter.
func strerror(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}

	return strings.ToUpper(s[:1]) + s[1:]
}
