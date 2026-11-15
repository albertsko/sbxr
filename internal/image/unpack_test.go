package image_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
)

func TestUnpackAppliesLayers(t *testing.T) {
	t.Parallel()

	img := hostImage(t,
		layer(t,
			dir("bin"),
			entry{name: su, typ: tar.TypeReg, body: "su", mode: 0o4755},
			entry{name: "bin/su-link", typ: tar.TypeLink, link: su},
			entry{name: "bin/sh", typ: tar.TypeSymlink, link: "busybox", mode: 0o777},
			entry{name: "home/alice/notes", typ: tar.TypeReg, body: "hi", mode: 0o600, uid: 1000, gid: 1000},
			entry{name: tmpDir, typ: tar.TypeDir, mode: 0o1777},
			entry{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
			entry{name: "run/fifo", typ: tar.TypeFifo, mode: 0o644},
			file("etc/gone", "old"),
			file("opq/lower", "old"),
			file("kept/lower", "old"),
		),
		layer(t,
			file("opq/upper", "new"),
			file("opq/.wh..wh..opq", ""),
			file("etc/.wh.gone", ""),
			file("kept/upper", "new"),
		),
	)

	root, err := unpack(t, img)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, at(root, su)); got != "su" {
		t.Errorf("bin/su = %q, want su", got)
	}

	for _, name := range []string{"dev/null", "run/fifo", "etc/gone", "opq/lower"} {
		if exists(at(root, name)) {
			t.Errorf("%s exists, want it skipped or removed", name)
		}
	}

	for _, name := range []string{"opq/upper", "kept/lower", "kept/upper"} {
		if !exists(at(root, name)) {
			t.Errorf("%s is missing", name)
		}
	}

	suInfo, _ := os.Stat(at(root, su))
	link, _ := os.Stat(at(root, "bin/su-link"))

	if !os.SameFile(suInfo, link) {
		t.Error("bin/su-link is not a hardlink of bin/su")
	}

	if suInfo.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("host mode of bin/su = %v, want no special bits on the host", suInfo.Mode())
	}

	if target, _ := os.Readlink(at(root, "bin/sh")); target != "busybox" {
		t.Errorf("bin/sh target = %q, want busybox", target)
	}

	tests := map[string]string{
		".":                "0:0:040755",
		su:                 "0:0:0104755",
		"home":             "0:0:040755",
		"home/alice/notes": "1000:1000:0100600",
		tmpDir:             "0:0:041777",
	}
	if runtime.GOOS == "darwin" {
		tests["bin/sh"] = "0:0:0120777"
	}

	for name, want := range tests {
		if got := overrideStat(t, at(root, name)); got != want {
			t.Errorf("override stat of %s = %q, want %q", name, got, want)
		}
	}
}

func TestUnpackRejectsEscapingPaths(t *testing.T) {
	t.Parallel()

	tests := map[string][]entry{
		"dot-dot":          {file("../escaped", "x")},
		"absolute symlink": {{name: "abs", typ: tar.TypeSymlink, link: "/"}, file("abs/escaped", "x")},
		"relative symlink": {{name: "rel", typ: tar.TypeSymlink, link: "../.."}, file("rel/escaped", "x")},
		"hardlink":         {{name: "hard", typ: tar.TypeLink, link: "../../escaped"}},
	}

	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, err := unpack(t, hostImage(t, layer(t, entries...)))
			if err == nil {
				t.Fatal("Unpack succeeded, want an error")
			}

			parent := filepath.Dir(root)
			if exists(filepath.Join(parent, "escaped")) || exists(filepath.Join(filepath.Dir(parent), "escaped")) {
				t.Error("a file was written outside the root")
			}
		})
	}
}

func TestUnpackCaseConflict(t *testing.T) {
	t.Parallel()

	tests := map[string][]entry{
		"files":       {file("etc/Hosts", "a"), file("etc/hosts", "b")},
		"directories": {file("usr/Lib/a", "a"), file("usr/lib/b", "b")},
	}

	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			insensitive := caseInsensitive(t, t.TempDir())
			_, err := unpack(t, hostImage(t, layer(t, entries...)))

			switch {
			case insensitive && !errors.Is(err, image.ErrCaseConflict):
				t.Fatalf("Unpack error = %v, want ErrCaseConflict", err)
			case !insensitive && err != nil:
				t.Fatalf("Unpack on a case-sensitive file system: %v", err)
			}
		})
	}
}

func TestUnpackChownSetsRealOwnership(t *testing.T) {
	t.Parallel()

	// Image ids need root, or a user namespace that maps them, which the run step sets up.
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}

	img := hostImage(t, layer(t,
		entry{name: "bin/tool", typ: tar.TypeReg, body: "x", mode: 0o4750, uid: 1000, gid: 1000},
		entry{name: tmpDir, typ: tar.TypeDir, mode: 0o1777},
	))

	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := (image.Unpacker{Ownership: image.OwnershipChown}).Unpack(img, root); err != nil {
		t.Fatal(err)
	}

	tool, _ := os.Stat(at(root, "bin/tool"))
	if want := os.ModeSetuid | 0o750; tool.Mode() != want {
		t.Errorf("bin/tool mode = %v, want %v", tool.Mode(), want)
	}

	if st, _ := tool.Sys().(*syscall.Stat_t); st.Uid != 1000 || st.Gid != 1000 {
		t.Errorf("bin/tool owner = %d:%d, want 1000:1000", st.Uid, st.Gid)
	}

	tmp, _ := os.Stat(filepath.Join(root, tmpDir))
	if want := os.ModeDir | os.ModeSticky | 0o777; tmp.Mode() != want {
		t.Errorf("tmp mode = %v, want %v", tmp.Mode(), want)
	}
}

func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()

	probe := filepath.Join(dir, "case-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return exists(filepath.Join(dir, "CASE-TEST"))
}
