package image

import (
	"context"
	"os"
	"path/filepath"

	"github.com/albertsko/sbxr/internal/runspec"
)

// Store is the image store of a run.
type Store struct {
	Cache *Cache
	// Overlay mounts each run root as an overlay of the cached root instead of cloning it.
	// It needs the mount privileges of a user namespace (Linux).
	Overlay bool
	// Progress, when set, receives the download progress of each pull.
	Progress func(Progress)
}

// Hold keeps prune from removing images while the returned lock file is open.
// The runner passes it to the VM process, so that the hold lasts while the VM runs, even when the runner dies.
func (s Store) Hold(ctx context.Context) (*os.File, error) {
	return s.Cache.Hold(ctx)
}

// Pull returns the image for ref, downloading it when policy allows.
func (s Store) Pull(ctx context.Context, ref string, policy runspec.Pull) (*Image, error) {
	return s.Cache.Pull(ctx, ref, policy, s.Progress)
}

// NewRoot creates the private run root dir/root and returns its path.
// The returned release function undoes what a removal of dir cannot, such as a mount.
func (s Store) NewRoot(img *Image, dir string) (string, func() error, error) {
	root := filepath.Join(dir, "root")

	if s.Overlay {
		release, err := img.MountRoot(dir, root)

		return root, release, err
	}

	return root, func() error { return nil }, img.CloneRoot(root)
}
