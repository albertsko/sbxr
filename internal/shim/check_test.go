//go:build linux

package shim

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCheck(t *testing.T) {
	t.Parallel()

	bundle := t.TempDir()
	for _, dir := range []string{"bin", "lib"} {
		err := os.Mkdir(filepath.Join(bundle, dir), 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}

	// A library that the host cannot load fails the check.
	err := os.WriteFile(filepath.Join(bundle, "lib", "libkrun.so.1"), []byte("not ELF"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, exe, want string
	}{
		{"no bundle", filepath.Join(bundle, "containerd-shim-sbxr-v1"), "is not in a bundle"},
		{"broken libkrun", filepath.Join(bundle, "bin", "containerd-shim-sbxr-v1"), "load " + filepath.Join(bundle, "lib", "libkrun.so.1")},
	}

	for _, tt := range tests {
		var stdout, stderr bytes.Buffer

		code := runCheck(tt.exe, &stdout, &stderr)
		if code != 1 || stdout.Len() > 0 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 1 and %q", tt.name, code, stdout.String(), stderr.String(), tt.want)
		}
	}
}

// TestMainCheckOnlyAlone checks that a containerd call, which always has flags, never runs the bundle check.
func TestMainCheckOnlyAlone(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := Main([]string{"-namespace", "k8s.io", "-id", "x", CheckArg}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `unknown action "check"`) {
		t.Errorf("exit %d, stderr %q; want the unknown action error", code, stderr.String())
	}
}
