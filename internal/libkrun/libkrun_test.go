//go:build darwin || linux

package libkrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindLibDir(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	onlyLibkrun := libDirWith(t, filepath.Join(base, "only-libkrun"), libkrunName)
	both := libDirWith(t, filepath.Join(base, "both"), libkrunName, krunfwName)
	later := libDirWith(t, filepath.Join(base, "later"), libkrunName, krunfwName)
	missing := filepath.Join(base, "missing")

	tests := []struct {
		name    string
		dirs    []string
		want    string
		wantErr bool
	}{
		{"first dir with both libraries", []string{missing, onlyLibkrun, both, later}, both, false},
		{"no dir with both libraries", []string{missing, onlyLibkrun}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := findLibDir(tt.dirs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("findLibDir() error = %v, wantErr %v", err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("findLibDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLibDirOverrideIsTheOnlyCandidate(t *testing.T) {
	t.Parallel()

	dir := libDirWith(t, t.TempDir(), libkrunName)

	_, err := LibDir(dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("LibDir(%q) error = %v, want an error that names the directory", dir, err)
	}
}

func TestEnvSetsLibraryPath(t *testing.T) {
	t.Parallel()

	got := Env("/opt/krun/lib")
	if len(got) != 1 || got[0] != libPathVar+"=/opt/krun/lib" {
		t.Errorf("Env() = %q", got)
	}
}

func TestCheckLibkrunRejectsNonLibrary(t *testing.T) {
	t.Parallel()

	dir := libDirWith(t, t.TempDir(), libkrunName, krunfwName)

	if err := CheckLibkrun(dir); err == nil {
		t.Error("CheckLibkrun() = nil, want error")
	}

	if _, err := CheckLibkrunfw(dir); err == nil {
		t.Error("CheckLibkrunfw() = nil, want error")
	}
}

func TestKrunfwVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"/usr/local/lib/libkrunfw.so.5.6.4", "5.6.4"},
		{"/home/linuxbrew/.linuxbrew/Cellar/libkrunfw/5.10.0/lib/libkrunfw.so.5.10.0", "5.10.0"},
		{"/opt/homebrew/Cellar/libkrunfw/5.6.4/lib/libkrunfw.5.dylib", "5.6.4"},
		{"/usr/local/lib/libkrunfw.5.dylib", ""},
		{"/usr/local/lib/libkrunfw.so.5", ""},
		{"/opt/libkrunfw/latest/lib/libkrunfw.so.5", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			v, ok := krunfwVersion(tt.path)

			got := ""
			if ok {
				got = v.String()
			}

			if got != tt.want {
				t.Errorf("krunfwVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		v    version
		want bool
	}{
		{version{5, 6, 3}, true},
		{version{5, 5, 9}, true},
		{version{4, 9, 9}, true},
		{version{5, 6, 4}, false},
		{version{5, 6, 10}, false},
		{version{5, 7, 0}, false},
		{version{6, 0, 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.v.String(), func(t *testing.T) {
			t.Parallel()

			if got := tt.v.less(minKrunfw); got != tt.want {
				t.Errorf("%s.less(%s) = %v, want %v", tt.v, minKrunfw, got, tt.want)
			}
		})
	}
}

func TestEnterRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	valid := Config{Root: "/root", CPUs: 1, MemoryMiB: 512}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"relative root", func(c *Config) { c.Root = "root" }},
		{"zero CPUs", func(c *Config) { c.CPUs = 0 }},
		{"zero memory", func(c *Config) { c.MemoryMiB = 0 }},
		{"negative network fd", func(c *Config) { c.Net = &Net{FD: -1} }},
		{"log level above trace", func(c *Config) { c.LogLevel = 6 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			tt.mutate(&cfg)

			// The directory has no libkrun, so a config that passes validation would fail to load instead.
			err := Enter(t.TempDir(), cfg)
			if err == nil || strings.Contains(err.Error(), "load ") {
				t.Errorf("Enter() error = %v, want a validation error", err)
			}
		})
	}
}

func TestValidateMountHelperPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "empty", path: "", wantErr: true},
		{name: "relative", path: ".sbxr-mount", wantErr: true},
		{name: "absolute", path: "/.sbxr-mount", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{Root: "/root", CPUs: 1, MemoryMiB: 512, Volumes: []Volume{{Tag: "v0", Host: "/data"}}, MountHelper: []byte("helper"), MountHelperPath: tt.path}

			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() with mount helper path %q = %v, want an error: %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func libDirWith(t *testing.T, dir string, names ...string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a library"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}
