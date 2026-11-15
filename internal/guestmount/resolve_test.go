package guestmount_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/albertsko/sbxr/internal/guestmount"
)

// guestRoot returns a directory with a few guest directories and symlinks.
func guestRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	for _, dir := range []string{"proc", "data/sub", "home/u"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	links := map[string]string{
		"alias":         "/proc",
		"rel":           "data/sub",
		"home/u/up":     "../../proc/self",
		"data/sub/back": "/data",
		"loop":          "loop",
		"escape":        "/missing/../alias",
		"escape2":       "/missing2/../../proc",
		"root-link":     "/",
		"data/up":       "..",
		"data/file":     "",
	}
	for name, target := range links {
		if target == "" {
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}

			continue
		}

		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func TestMountPoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
		created  []string
	}{
		{in: "/work", want: "/work", created: []string{"work"}},
		{in: "/work/new/dir", want: "/work/new/dir", created: []string{"work/new/dir"}},
		{in: "/data/sub", want: "/data/sub"},
		{in: "/rel/x", want: "/data/sub/x", created: []string{"data/sub/x"}},
		{in: "/data/sub/back/sub", want: "/data/sub"},
		{in: "/missing/../work", want: "/work", created: []string{"missing", "work"}},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			root := guestRoot(t)

			got, err := guestmount.MountPoint(root, tt.in)
			if err != nil || got != tt.want {
				t.Fatalf("MountPoint(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
			}

			for _, dir := range append(tt.created, strings.TrimPrefix(tt.want, "/")) {
				if info, err := os.Stat(filepath.Join(root, dir)); err != nil || !info.IsDir() {
					t.Errorf("directory %s is missing: %v", dir, err)
				}
			}
		})
	}
}

func TestMountPointRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
		err      error
	}{
		{in: "/alias", want: "reserved path /proc"},
		{in: "/alias/x/y", want: "reserved path /proc"},
		{in: "/home/u/up", want: "reserved path /proc"},
		{in: "/data/../alias", want: "reserved path /proc"},
		{in: "/.sbxr-mount/x", want: "reserved path " + guestmount.Path},
		{in: "/escape", want: "reserved path /proc"},
		{in: "/escape2/x", want: "reserved path /proc"},
		{in: "/root-link", want: "resolves to the guest root"},
		{in: "/data/up", want: "resolves to the guest root"},
		{in: "/loop/x", err: syscall.ELOOP},
		{in: "/data/file/x", err: syscall.ENOTDIR},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			root := guestRoot(t)

			got, err := guestmount.MountPoint(root, tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("MountPoint(%q) = %q, %v, want an error with %q", tt.in, got, err, tt.want)
			}

			// Nothing is created inside a reserved path.
			if entries, _ := os.ReadDir(filepath.Join(root, "proc")); len(entries) > 0 {
				t.Errorf("MountPoint(%q) created %v in /proc", tt.in, entries)
			}
		})
	}
}

func TestReserved(t *testing.T) {
	t.Parallel()

	for p, want := range map[string]string{"/proc": "/proc", "/proc/1": "/proc", "/procs": "", "/work": "", guestmount.Path: guestmount.Path} {
		if got := guestmount.Reserved(p); got != want {
			t.Errorf("Reserved(%q) = %q, want %q", p, got, want)
		}
	}
}
