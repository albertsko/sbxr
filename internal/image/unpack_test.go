package image_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	v1 "github.com/google/go-containerregistry/pkg/v1"
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

// TestUnpackWhiteoutsKeepTheirOwnLayer checks that whiteouts remove only lower layers, in either entry order.
func TestUnpackWhiteoutsKeepTheirOwnLayer(t *testing.T) {
	t.Parallel()

	lower := []entry{file("foo", "old"), file("opq/old", "old"), file("opq/sub/old", "old"), file("dir/old", "old")}
	upper := []entry{
		file("foo", "new"), file(".wh.foo", ""),
		file("opq/sub/new", "new"), file("opq/.wh..wh..opq", ""),
		file("dir/new", "new"), file(".wh.dir", ""),
	}
	reversed := slices.Clone(upper)
	slices.Reverse(reversed)

	for name, top := range map[string][]entry{"entry first": upper, "whiteout first": reversed} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, err := unpack(t, hostImage(t, layer(t, lower...), layer(t, top...)))
			if err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{"foo", "opq/sub/new", "dir/new"} {
				if got := readFile(t, at(root, name)); got != "new" {
					t.Errorf("%s = %q, want new", name, got)
				}
			}

			for _, name := range []string{"opq/old", "opq/sub/old", "dir/old"} {
				if exists(at(root, name)) {
					t.Errorf("%s exists, want the whiteout to remove it", name)
				}
			}
		})
	}
}

// TestUnpackSameLayerWhiteoutOfASymlink checks that a whiteout of a symlink that its own layer wrote never follows it.
func TestUnpackSameLayerWhiteoutOfASymlink(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"data", "/data"} {
		top := []entry{symlink("foo", target), file(".wh.foo", "")}
		reversed := slices.Clone(top)
		slices.Reverse(reversed)

		for order, entries := range map[string][]entry{"symlink first": top, "whiteout first": reversed} {
			t.Run(target+"/"+order, func(t *testing.T) {
				t.Parallel()

				root, err := unpack(t, hostImage(t, layer(t, file("data/keep", "x")), layer(t, entries...)))
				if err != nil {
					t.Fatal(err)
				}

				if got := readFile(t, at(root, "data/keep")); got != "x" {
					t.Errorf("data/keep = %q, want x", got)
				}

				if link, err := os.Readlink(at(root, "foo")); err != nil || link != target {
					t.Errorf("foo = %q, %v; want the symlink to %s from its own layer", link, err, target)
				}
			})
		}
	}
}

func TestUnpackRejectsEscapingPaths(t *testing.T) {
	t.Parallel()

	tests := map[string][]entry{
		"dot-dot":  {file("../escaped", "x")},
		"hardlink": {{name: "hard", typ: tar.TypeLink, link: "../../escaped"}},
	}

	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, err := unpack(t, hostImage(t, layer(t, entries...)))
			if err == nil {
				t.Fatal("Unpack succeeded, want an error")
			}

			assertNotOutside(t, root)
		})
	}
}

func TestUnpackResolvesSymlinksInRoot(t *testing.T) {
	t.Parallel()

	varRun := symlink("var/run", "/run")

	tests := map[string]struct {
		layers [][]entry
		want   []string
		gone   []string
	}{
		"later layer through absolute symlink": {
			layers: [][]entry{{dir("run"), varRun}, {file("var/run/app.pid", "1")}},
			want:   []string{"run/app.pid", varRun.name},
		},
		"whiteout through symlinked parent": {
			layers: [][]entry{{file("run/old", "x"), varRun}, {file("var/run/.wh.old", "")}},
			want:   []string{"run", varRun.name},
			gone:   []string{"run/old"},
		},
		"whiteout removes the symlink itself": {
			layers: [][]entry{{file("run/keep", "x"), varRun}, {file("var/.wh.run", "")}},
			want:   []string{"run/keep"},
			gone:   []string{varRun.name},
		},
		"opaque whiteout through symlinked directory": {
			layers: [][]entry{{file("data/old", "x"), symlink("link", "/data")}, {file("link/.wh..wh..opq", ""), file("data/new", "y")}},
			want:   []string{"data/new", "link"},
			gone:   []string{"data/old"},
		},
		"absolute symlink to root": {
			layers: [][]entry{{symlink("abs", "/"), file("abs/escaped", "x")}},
			want:   []string{"escaped"},
		},
		"relative symlink above root": {
			layers: [][]entry{{symlink("rel", "../.."), file("rel/escaped", "x")}},
			want:   []string{"escaped"},
		},
		"dot-dot above root in absolute symlink": {
			layers: [][]entry{{symlink("cfg", "/../../etc"), file("cfg/x", "x")}},
			want:   []string{"etc/x"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			layers := make([]v1.Layer, 0, len(tt.layers))
			for _, entries := range tt.layers {
				layers = append(layers, layer(t, entries...))
			}

			root, err := unpack(t, hostImage(t, layers...))
			if err != nil {
				t.Fatal(err)
			}

			for _, name := range tt.want {
				if !exists(at(root, name)) {
					t.Errorf("%s is missing", name)
				}
			}

			for _, name := range tt.gone {
				if exists(at(root, name)) {
					t.Errorf("%s exists, want it removed", name)
				}
			}

			assertNotOutside(t, root)
		})
	}

	t.Run("entry replaces a symlink without writing its target", func(t *testing.T) {
		t.Parallel()

		root, err := unpack(t, hostImage(t,
			layer(t, file("etc/real", "old"), symlink("etc/conf", "/etc/real")),
			layer(t, file("etc/conf", "new")),
		))
		if err != nil {
			t.Fatal(err)
		}

		if got := readFile(t, at(root, "etc/real")); got != "old" {
			t.Errorf("etc/real = %q, want old", got)
		}
	})
}

func TestUnpackSymlinkLoop(t *testing.T) {
	t.Parallel()

	_, err := unpack(t, hostImage(t, layer(t, symlink("a", "b"), symlink("b", "/a"), file("a/x", "x"))))
	if err == nil || !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Fatalf("Unpack error = %v, want a symlink loop error", err)
	}
}

func TestUnpackCaseConflict(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		layers   [][]entry
		conflict bool
	}{
		"files":                {layers: [][]entry{{file("etc/Hosts", "a"), file("etc/hosts", "b")}}, conflict: true},
		"directories":          {layers: [][]entry{{file("usr/Lib/a", "a"), file("usr/lib/b", "b")}}, conflict: true},
		"across layers":        {layers: [][]entry{{file("etc/hosts", "a")}, {file("etc/Hosts", "b")}}, conflict: true},
		"after whiteout":       {layers: [][]entry{{file("etc/hosts", "a")}, {file("etc/.wh.hosts", ""), file("etc/Hosts", "b")}}},
		"same name overridden": {layers: [][]entry{{file("etc/hosts", "a")}, {file("etc/hosts", "b")}}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			layers := make([]v1.Layer, 0, len(tt.layers))
			for _, entries := range tt.layers {
				layers = append(layers, layer(t, entries...))
			}

			insensitive := caseInsensitive(t, t.TempDir())
			_, err := unpack(t, hostImage(t, layers...))

			switch {
			case insensitive && tt.conflict && !errors.Is(err, image.ErrCaseConflict):
				t.Fatalf("Unpack error = %v, want ErrCaseConflict", err)
			case (!insensitive || !tt.conflict) && err != nil:
				t.Fatalf("Unpack: %v", err)
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

// assertNotOutside fails when an unpack wrote a file named escaped next to the root or one level above it.
func assertNotOutside(t *testing.T, root string) {
	t.Helper()

	parent := filepath.Dir(root)
	if exists(filepath.Join(parent, "escaped")) || exists(filepath.Join(filepath.Dir(parent), "escaped")) {
		t.Error("a file was written outside the root")
	}
}
