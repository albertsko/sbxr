package run

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// checkVolumes checks that each host path is a directory and that this build can mount volumes.
func (r *Runner) checkVolumes(volumes []spec.Volume) error {
	if len(volumes) == 0 {
		return nil
	}

	for _, v := range volumes {
		info, err := os.Stat(v.Host)
		if err != nil || !info.IsDir() {
			return &spec.Error{Source: "-v", Value: v.Host, Problem: "the host path is not a directory", Fix: "create the directory or fix the path"}
		}
	}

	err := r.MountHelper()
	if errors.Is(err, guestmount.ErrMissing) {
		return &spec.Error{Source: "-v", Problem: err.Error(), Fix: "build sbxr with mise run build"}
	}

	if err != nil {
		return fmt.Errorf("mount helper: %w", err)
	}

	return nil
}

// withVolumes makes the guest init run the mount helper as root before the workload.
// The helper then applies the user, groups, and working directory of cfg. Without volumes, cfg stays unchanged.
func withVolumes(volumes []spec.Volume, cfg krunconfig.Config) (krunconfig.Config, []vm.Volume) {
	if len(volumes) == 0 {
		return cfg, nil
	}

	// A volume inside another one mounts after it.
	sorted := slices.SortedFunc(slices.Values(volumes), func(a, b spec.Volume) int { return cmp.Compare(a.Guest, b.Guest) })

	mounts := make([]guestmount.Mount, 0, len(sorted))
	devices := make([]vm.Volume, 0, len(sorted))

	for _, v := range sorted {
		// The guest path is unique, so its hash is a unique tag.
		sum := sha256.Sum256([]byte(v.Guest))
		tag := "sbxr-" + hex.EncodeToString(sum[:8])

		mounts = append(mounts, guestmount.Mount{Tag: tag, Path: v.Guest, ReadOnly: v.ReadOnly})
		devices = append(devices, vm.Volume{Tag: tag, Host: v.Host, ReadOnly: v.ReadOnly})
	}

	cfg.Args = guestmount.Encode(guestmount.Args{UID: cfg.UID, GID: cfg.GID, WorkingDir: cfg.WorkingDir, Mounts: mounts, Argv: cfg.Args})
	cfg.UID, cfg.GID = 0, 0

	return cfg, devices
}
