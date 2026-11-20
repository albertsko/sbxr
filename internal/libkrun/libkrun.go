//go:build darwin || linux

// Package libkrun runs a VM with libkrun, loaded at runtime through purego without cgo.
//
// libkrun takes over the process that starts the VM, so [Enter] runs in a dedicated VM process.
// libkrun loads libkrunfw by bare name, so that process must start with the environment from [Env].
package libkrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ebitengine/purego"
)

// minKrunfw is the oldest supported libkrunfw version.
var minKrunfw = version{5, 6, 4}

// libkrun 1.x loads libkrunfw with this ABI version.
const krunfwABI = 5

// LibDir returns the directory that holds both libraries.
// A non-empty override, the SBXR_LIB_DIR value, is the only candidate.
// Otherwise LibDir returns the first default directory that holds both libraries.
func LibDir(override string) (string, error) {
	if override != "" {
		return findLibDir([]string{override})
	}

	return findLibDir(defaultLibDirs())
}

func findLibDir(dirs []string) (string, error) {
	for _, dir := range dirs {
		if isFile(filepath.Join(dir, libkrunName)) && isFile(filepath.Join(dir, krunfwName)) {
			return dir, nil
		}
	}

	return "", fmt.Errorf("%s and %s not found in %s", libkrunName, krunfwName, strings.Join(dirs, ", "))
}

func isFile(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // The library directory is meant to come from the user.

	return err == nil && info.Mode().IsRegular()
}

// Env returns the environment entries that let libkrun in the VM process find libkrunfw in dir.
func Env(dir string) []string {
	return []string{libPathVar + "=" + dir}
}

// CheckLibkrun loads libkrun from dir and checks that it exports every required symbol.
func CheckLibkrun(dir string) error {
	_, err := load(dir)

	return err
}

// CheckLibkrunfw loads libkrunfw from dir and returns its version.
// It fails when the version is older than 5.6.4 or cannot be determined.
func CheckLibkrunfw(dir string) (string, error) {
	path := filepath.Join(dir, krunfwName)

	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", path, err)
	}

	var getVersion func() int32
	if err := register(handle, "krunfw_get_version", &getVersion); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	if abi := getVersion(); abi != krunfwABI {
		return "", fmt.Errorf("%s reports ABI version %d, want %d", path, abi, krunfwABI)
	}

	// krunfw_get_version reports only the ABI version, so the full version comes from the installed file path.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	ver, ok := krunfwVersion(resolved)
	if !ok {
		return "", fmt.Errorf("cannot determine the libkrunfw version from %s", resolved)
	}

	if ver.less(minKrunfw) {
		return "", fmt.Errorf("libkrunfw %s is older than %s", ver, minKrunfw)
	}

	return ver.String(), nil
}

// krunfwVersion reads the version from a file name such as libkrunfw.so.5.6.4,
// or from a Homebrew path such as Cellar/libkrunfw/5.6.4/lib/libkrunfw.5.dylib.
func krunfwVersion(path string) (version, bool) {
	if v, ok := parseVersion(strings.TrimPrefix(filepath.Base(path), "libkrunfw.so.")); ok {
		return v, true
	}

	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range len(parts) - 1 {
		if parts[i] != "libkrunfw" {
			continue
		}

		if v, ok := parseVersion(parts[i+1]); ok {
			return v, true
		}
	}

	return version{}, false
}

type version [3]int

func parseVersion(s string) (version, bool) {
	fields := strings.Split(s, ".")
	if len(fields) != len(version{}) {
		return version{}, false
	}

	var v version

	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return version{}, false
		}

		v[i] = n
	}

	return v, true
}

func (v version) less(w version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}

	return false
}

func (v version) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// register binds fn to the exported symbol name, so that a missing symbol is an error instead of a panic.
func register(handle uintptr, name string, fn any) error {
	addr, err := purego.Dlsym(handle, name)
	if err != nil {
		return errors.New("missing symbol " + name)
	}

	purego.RegisterFunc(fn, addr)

	return nil
}
