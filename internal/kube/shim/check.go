//go:build linux

package shim

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/albertsko/sbxr/internal/vm/libkrun"
)

// CheckArg is the hidden only argument that loads libkrun and libkrunfw from the shim's bundle and exits.
// The node installer runs it on the host after each install, so a bundle that the host cannot load never gets a ready node.
// containerd always passes flags before an action, so it never calls the shim with this argument alone.
const CheckArg = "check"

// runCheck loads the libraries from <bundle>/lib, the same directory that the VM process uses, for the shim at exe.
func runCheck(exe string, stdout, stderr io.Writer) int {
	err := checkBundle(exe, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "containerd-shim-sbxr-v1 check: %v\n", err)

		return 1
	}

	return 0
}

func checkBundle(exe string, stdout io.Writer) error {
	bin := filepath.Dir(exe)
	if filepath.Base(bin) != "bin" {
		return fmt.Errorf("shim binary %s is not in a bundle", exe)
	}

	libDir := filepath.Join(filepath.Dir(bin), "lib")

	err := libkrun.CheckLibkrun(libDir)
	if err != nil {
		return err //nolint:wrapcheck // The error names the library.
	}

	version, err := libkrun.CheckLibkrunfw(libDir)
	if err != nil {
		return err //nolint:wrapcheck // The error names the library.
	}

	_, err = fmt.Fprintf(stdout, "libkrun ok, libkrunfw %s\n", version)
	if err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	return nil
}

// executable returns the path of the running shim binary, with symbolic links resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find shim binary: %w", err)
	}

	return exe, nil
}
