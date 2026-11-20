//go:build vm

package e2e

import (
	"strings"
	"testing"
)

// TestVMCommandPaths checks how the guest finds the command: a directory is not executable, and a relative path starts at the workdir.
func TestVMCommandPaths(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

	tests := []struct {
		name     string
		args     []string
		wantCode int
		// wantStdout is a part of stdout. The guest init prints its exec errors there.
		wantStdout string
	}{
		{name: "directory", args: []string{busybox, "/bin"}, wantCode: 126, wantStdout: "Permission denied"},
		{name: "relative path from the workdir", args: []string{"-w", "/bin", busybox, "./echo", "hi"}, wantStdout: "hi\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cacheDir := vmCacheDir(t)

			res := sbxr(t, bin, vmEnv(cacheDir), runArgs(tt.args...)...)
			if res.code != tt.wantCode || !strings.Contains(res.stdout, tt.wantStdout) {
				t.Errorf("sbxr run %q = %d, stdout %q, stderr %q; want %d and %q", tt.args, res.code, res.stdout, res.stderr, tt.wantCode, tt.wantStdout)
			}

			err := checkClean(cacheDir)
			if err != nil {
				t.Error(err)
			}
		})
	}
}
