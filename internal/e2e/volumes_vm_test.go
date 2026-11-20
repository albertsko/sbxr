//go:build vm

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVMVolumeSymlinksCannotEscape mounts a volume through symlinks in an earlier volume that lead to a reserved path,
// the guest root, or through a file.
func TestVMVolumeSymlinksCannotEscape(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)
	cacheDir := vmCacheDir(t)

	links, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(links, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, to := range map[string]string{"escape": "/missing/../proc", "root": "/", "edge": "/a/file/../ok"} {
		if err := os.Symlink(to, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}

	for name, want := range map[string]string{"escape": "reserved path /proc", "root": "resolves to the guest root", "edge": "not a directory"} {
		res := sbxr(t, bin, vmEnv(cacheDir), runArgs("-q", "-v", links+":/a", "-v", target+":/a/"+name, refs[Busybox], "true")...)
		if res.code != 125 || !strings.Contains(res.stderr, want) {
			t.Errorf("volume at /a/%s: sbxr run = %d, stderr %q; want 125 and %q", name, res.code, res.stderr, want)
		}
	}

	if err := checkClean(cacheDir); err != nil {
		t.Error(err)
	}
}
