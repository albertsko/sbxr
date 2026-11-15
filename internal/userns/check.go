//go:build linux

package userns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// CheckArg is the hidden first argument that runs [CheckInside].
const CheckArg = "__userns-check"

// Check enters a new namespace and checks that a chown to uid 0 gid 42 and a private mount succeed there.
func Check(ctx context.Context) error {
	var out bytes.Buffer

	cmd := exec.CommandContext(ctx, "/proc/self/exe", CheckArg)
	cmd.Stdout, cmd.Stderr = &out, &out

	err := Start(ctx, cmd)
	if err != nil {
		return err
	}

	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("in a new user namespace: %s", strings.TrimSpace(out.String()))
	}

	return nil
}

// CheckInside runs the [Check] steps in the namespace and returns the exit code.
func CheckInside() int {
	err := checkInside()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err) // The exit code reports the failure too.

		return 1
	}

	return 0
}

func checkInside() error {
	if !Inside() {
		return errors.New("not inside a user namespace")
	}

	dir, err := os.MkdirTemp("", "sbxr-userns-")
	if err != nil {
		return fmt.Errorf("create test directory: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // A leftover test directory in the temporary directory is harmless.

	file := filepath.Join(dir, "file")

	err = os.WriteFile(file, nil, 0o600)
	if err != nil {
		return fmt.Errorf("create test file: %w", err)
	}

	err = os.Chown(file, 0, 42)
	if err != nil {
		return fmt.Errorf("chown to 0:42: %w", err)
	}

	mnt := filepath.Join(dir, "mnt")

	err = os.Mkdir(mnt, 0o700)
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}

	err = unix.Mount("tmpfs", mnt, "tmpfs", 0, "")
	if err != nil {
		return fmt.Errorf("mount tmpfs: %w", err)
	}

	err = unix.Mount("", mnt, "", unix.MS_PRIVATE, "")
	if err != nil {
		return fmt.Errorf("make mount private: %w", err)
	}

	err = unix.Unmount(mnt, 0)
	if err != nil {
		return fmt.Errorf("unmount tmpfs: %w", err)
	}

	return nil
}
