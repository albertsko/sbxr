package guestmount_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/albertsko/sbxr/internal/guestmount"
)

func TestResolve(t *testing.T) {
	t.Parallel()

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
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		in, want, reserved string
	}{
		{in: "/work", want: "/work"},
		{in: "/work/new/dir", want: "/work/new/dir"},
		{in: "/data/sub", want: "/data/sub"},
		{in: "/alias", want: "/proc", reserved: "/proc"},
		{in: "/alias/x/y", want: "/proc/x/y", reserved: "/proc"},
		{in: "/rel/x", want: "/data/sub/x"},
		{in: "/home/u/up", want: "/proc/self", reserved: "/proc"},
		{in: "/data/sub/back/sub", want: "/data/sub"},
		{in: "/data/../alias", want: "/proc", reserved: "/proc"},
		{in: "/.sbxr-mount/x", want: "/.sbxr-mount/x", reserved: guestmount.Path},
	}

	for _, tt := range tests {
		got, err := guestmount.Resolve(root, tt.in)
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}

		if r := guestmount.Reserved(got); r != tt.reserved {
			t.Errorf("Reserved(%q) = %q, want %q", got, r, tt.reserved)
		}
	}

	if _, err := guestmount.Resolve(root, "/loop/x"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("Resolve of a symlink loop: %v, want ELOOP", err)
	}
}
