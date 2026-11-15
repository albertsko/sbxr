package image_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestCloneRootIsolatesRuns(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	ref := reg.push(t, "app", hostImage(t, layer(t,
		file("etc/config", "original"),
		file("etc/remove-me", "x"),
		entry{name: su, typ: tar.TypeReg, body: "su", mode: 0o4755},
		entry{name: "bin/su-link", typ: tar.TypeLink, link: su},
	)))
	c, cacheDir := openCache(t)

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	run := filepath.Join(cacheDir, "runs", "run1", "root")
	if err := os.MkdirAll(filepath.Dir(run), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := img.CloneRoot(run); err != nil {
		t.Fatal(err)
	}

	if got := overrideStat(t, at(run, su)); got != "0:0:0104755" {
		t.Errorf("override stat of bin/su in the run root = %q, want 0:0:0104755", got)
	}

	suInfo, _ := os.Stat(at(run, su))
	link, _ := os.Stat(at(run, "bin/su-link"))

	if !os.SameFile(suInfo, link) {
		t.Error("bin/su-link in the run root is not a hardlink of bin/su")
	}

	mustWrite(t, at(run, "etc/config"), "changed")
	mustWrite(t, at(run, "etc/new"), "new")
	mustWrite(t, at(run, "bin/su-link"), "changed")

	if err := os.Remove(at(run, "etc/remove-me")); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, at(img.Root, "etc/config")); got != "original" {
		t.Errorf("cached etc/config = %q, want original", got)
	}

	if got := readFile(t, at(img.Root, su)); got != "su" {
		t.Errorf("cached bin/su = %q, want su", got)
	}

	if !exists(at(img.Root, "etc/remove-me")) || exists(at(img.Root, "etc/new")) {
		t.Error("a change in the run root reached the cache")
	}

	next := filepath.Join(cacheDir, "runs", "run2")
	if err := img.CloneRoot(next); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, at(next, "etc/config")); got != "original" {
		t.Errorf("etc/config in the next run = %q, want original", got)
	}
}

func TestResolveUser(t *testing.T) {
	t.Parallel()

	img := userImage(t, layer(t,
		file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n# comment\nalice:x:1000:1001::/home/alice:/bin/sh\nbob:x:1002:1003::/:/bin/sh\n"),
		file("etc/group", "root:x:0:\nstaff:x:50:alice\n"),
	))

	tests := []struct {
		user     string
		uid, gid uint32
		err      string
	}{
		{user: "", uid: 0, gid: 0},
		{user: "root", uid: 0, gid: 0},
		{user: "alice", uid: 1000, gid: 1001},
		{user: "1000", uid: 1000, gid: 1001},
		{user: "4242", uid: 4242, gid: 0},
		{user: "1000:50", uid: 1000, gid: 50},
		{user: "alice:staff", uid: 1000, gid: 50},
		{user: "4242:staff", uid: 4242, gid: 50},
		{user: "carol", err: "no such user"},
		{user: "alice:nogroup", err: "no such group"},
		{user: "-1", err: "no such user"},
	}

	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			t.Parallel()

			uid, gid, err := img.ResolveUser(tt.user)

			switch {
			case tt.err != "":
				if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.Contains(err.Error(), tt.user) {
					t.Errorf("ResolveUser(%q) error = %v, want one that names the user and says %q", tt.user, err, tt.err)
				}
			case err != nil:
				t.Errorf("ResolveUser(%q): %v", tt.user, err)
			case uid != tt.uid || gid != tt.gid:
				t.Errorf("ResolveUser(%q) = %d:%d, want %d:%d", tt.user, uid, gid, tt.uid, tt.gid)
			}
		})
	}
}

func TestResolveUserFollowsSymlinksInRoot(t *testing.T) {
	t.Parallel()

	passwd := file("usr/share/passwd", "alice:x:1000:1000::/:/bin/sh\n")
	tests := map[string]struct {
		entries []entry
		ok      bool
	}{
		"absolute":            {entries: []entry{passwd, symlink("etc/passwd", "/usr/share/passwd")}, ok: true},
		"relative above root": {entries: []entry{passwd, symlink("etc/passwd", "../../../../../usr/share/passwd")}, ok: true},
		"symlinked parent":    {entries: []entry{passwd, symlink("etc", "/usr/share")}, ok: true},
		"host path":           {entries: []entry{symlink("etc/passwd", "/etc/master.passwd")}},
		"loop":                {entries: []entry{symlink("etc/passwd", "/etc/loop"), symlink("etc/loop", "/etc/passwd")}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			img := userImage(t, layer(t, tt.entries...))
			uid, _, err := img.ResolveUser("alice")

			switch {
			case tt.ok && (err != nil || uid != 1000):
				t.Errorf("ResolveUser(alice) = %d, %v, want 1000", uid, err)
			case !tt.ok && err == nil:
				t.Error("ResolveUser(alice) succeeded, want an error")
			}
		})
	}
}

func TestResolveUserWithoutPasswd(t *testing.T) {
	t.Parallel()

	img := userImage(t, layer(t, file("bin/app", "x")))

	if uid, gid, err := img.ResolveUser("1000"); err != nil || uid != 1000 || gid != 0 {
		t.Errorf("ResolveUser(1000) = %d:%d, %v, want 1000:0", uid, gid, err)
	}

	if _, _, err := img.ResolveUser("alice"); err == nil {
		t.Error("ResolveUser(alice) succeeded without /etc/passwd")
	}
}

func userImage(t *testing.T, l v1.Layer) *image.Image {
	t.Helper()

	reg := newRegistry(t)
	ref := reg.push(t, "users", hostImage(t, l))
	c, _ := openCache(t)

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	return img
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
