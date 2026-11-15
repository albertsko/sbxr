// Package guestmount describes the guest mount helper for -v.
//
// The libkrun init cannot mount devices and drops to the workload user before it runs the workload.
// With volumes, libkrun serves the helper in the root share from memory, and the init runs it as root. The helper mounts each virtio-fs device,
// then applies the user, groups, and working directory like the init and runs the workload.
// Runs without volumes do not use the helper.
package guestmount

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

const (
	// Path is the helper in the guest root. libkrun serves it from memory, so the run root has no such file.
	Path = "/.sbxr-mount"
	// FileName is the helper's file name in the generated helper directory.
	FileName = "sbxr-mount"
)

// ErrMissing means that this sbxr build has no helper, because `go generate` did not run before the build.
var ErrMissing = errors.New("this sbxr build has no mount helper")

// Mount is one virtio-fs device to mount in the guest.
type Mount struct {
	Tag      string
	Path     string
	ReadOnly bool
}

// Args is the job of the helper.
type Args struct {
	UID, GID   uint32
	WorkingDir string
	Mounts     []Mount
	// Argv is the workload command line.
	Argv []string
}

// Encode returns the helper command line: Path, "UID:GID", the working directory, the mount count,
// one "TAG:rw:PATH" or "TAG:ro:PATH" entry per mount, and the workload command line.
func Encode(a Args) []string {
	argv := []string{Path, fmt.Sprintf("%d:%d", a.UID, a.GID), a.WorkingDir, strconv.Itoa(len(a.Mounts))}

	for _, m := range a.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}

		argv = append(argv, m.Tag+":"+mode+":"+m.Path)
	}

	return append(argv, a.Argv...)
}

// Decode parses a command line from Encode.
func Decode(argv []string) (Args, error) {
	var a Args

	if len(argv) < 4 {
		return a, errors.New("too few arguments")
	}

	uid, gid, ok := strings.Cut(argv[1], ":")
	u, err1 := strconv.ParseUint(uid, 10, 32)
	g, err2 := strconv.ParseUint(gid, 10, 32)
	n, err3 := strconv.Atoi(argv[3])

	if !ok || err1 != nil || err2 != nil || err3 != nil || n < 0 || len(argv) < 4+n+1 {
		return a, fmt.Errorf("invalid arguments %q", argv)
	}

	a.UID, a.GID, a.WorkingDir = uint32(u), uint32(g), argv[2]

	for _, entry := range argv[4 : 4+n] {
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 || (parts[1] != "rw" && parts[1] != "ro") {
			return a, fmt.Errorf("invalid mount %q", entry)
		}

		a.Mounts = append(a.Mounts, Mount{Tag: parts[0], Path: parts[2], ReadOnly: parts[1] == "ro"})
	}

	a.Argv = argv[4+n:]

	return a, nil
}

// Binary returns the helper for linux on the host architecture, which is also the guest architecture.
func Binary() ([]byte, error) {
	data, err := helper.ReadFile(helperDir + "/" + FileName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMissing
	}

	if err != nil {
		return nil, fmt.Errorf("read mount helper: %w", err)
	}

	return data, nil
}
