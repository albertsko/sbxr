package scenario

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// hostFile is a file that the volume scenarios put into the host directory.
const (
	workDir     = "/work"
	hostFile    = "from-host"
	hostContent = "host data\n"
)

// volumeDir creates a host directory with hostFile for one scenario and returns it with its removal.
func volumeDir() (string, func(), error) {
	dir, err := os.MkdirTemp("", "sbxr-volume-")
	if err != nil {
		return "", nil, fmt.Errorf("volume directory: %w", err)
	}

	remove := func() { _ = os.RemoveAll(dir) }

	err = os.WriteFile(filepath.Join(dir, hostFile), []byte(hostContent), 0o644) //nolint:gosec // The guest reads the file.
	if err != nil {
		remove()

		return "", nil, fmt.Errorf("volume directory: %w", err)
	}

	return dir, remove, nil
}

// volumeReadWrite reads a host file in the volume and creates a file that must appear on the host.
// The working directory is the mount point, which exists only after the mount.
func volumeReadWrite(ctx context.Context, t Target) error {
	dir, remove, err := volumeDir()
	if err != nil {
		return err
	}
	defer remove()

	w := Workload{
		Image:   Busybox,
		Volumes: []Volume{{Host: dir, Guest: workDir}},
		Workdir: workDir,
		Args:    []string{"sh", "-c", "cat " + hostFile + "; touch from-guest; pwd"},
	}

	err = expect(w, 0, OK, stdout(hostContent+workDir+"\n"))(ctx, t)
	if err != nil {
		return err
	}

	_, err = os.Stat(filepath.Join(dir, "from-guest"))
	if err != nil {
		return fmt.Errorf("the file from the guest is missing on the host: %w", err)
	}

	return nil
}

// volumeReadOnly reads a host file in a read-only volume and fails to create a file.
func volumeReadOnly(ctx context.Context, t Target) error {
	dir, remove, err := volumeDir()
	if err != nil {
		return err
	}
	defer remove()

	w := Workload{
		Image:   Busybox,
		Volumes: []Volume{{Host: dir, Guest: "/data", ReadOnly: true}},
		Args:    []string{"sh", "-c", "cat /data/" + hostFile + "; touch /data/from-guest"},
	}

	err = expect(w, 1, OK, stdout(hostContent))(ctx, t)
	if err != nil {
		return err
	}

	_, err = os.Stat(filepath.Join(dir, "from-guest"))
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the guest wrote to a read-only volume: %w", err)
	}

	return nil
}

// withVolume runs check with a read-only volume added to the workload of check, so that scenarios run once with the mount helper.
func withVolume(w Workload, code int, class ErrorClass, checks ...resultCheck) func(context.Context, Target) error {
	return func(ctx context.Context, t Target) error {
		dir, remove, err := volumeDir()
		if err != nil {
			return err
		}
		defer remove()

		w.Volumes = []Volume{{Host: dir, Guest: "/mnt/volume", ReadOnly: true}}

		return expect(w, code, class, checks...)(ctx, t)
	}
}

// invalidVolume expects that the run input with flag is rejected before anything starts.
func invalidVolume(v Volume) func(context.Context, Target) error {
	return func(ctx context.Context, t Target) error {
		dir, remove, err := volumeDir()
		if err != nil {
			return err
		}
		defer remove()

		if v.Host == "" {
			v.Host = dir
		}

		return expect(Workload{Image: Busybox, Volumes: []Volume{v}, Args: []string{"pwd"}}, 125, InvalidSpec)(ctx, t)
	}
}
