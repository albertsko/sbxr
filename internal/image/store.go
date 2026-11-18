package image

import (
	"context"
	"os"
	"path/filepath"

	"github.com/albertsko/sbxr/internal/run"
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

// Hold holds the cache, so that prune cannot remove images while the run lasts.
func (s Store) Hold(ctx context.Context) (*os.File, error) {
	return s.Cache.Hold(ctx)
}

// Pull returns the image for ref. The image ID is its cached root directory.
func (s Store) Pull(ctx context.Context, ref string, policy runspec.Pull) (run.Image, error) {
	img, err := s.Cache.Pull(ctx, ref, PullPolicy(policy), s.Progress)
	if err != nil {
		return run.Image{}, err
	}

	return run.Image{ID: img.Root, Config: img.Config}, nil
}

// ResolveUser resolves user through the image /etc/passwd and /etc/group.
func (s Store) ResolveUser(img run.Image, user string) (uint32, uint32, error) {
	return cached(img).ResolveUser(user)
}

// NewRoot creates the run root dir/root.
func (s Store) NewRoot(_ context.Context, img run.Image, dir string) (string, func() error, error) {
	root := filepath.Join(dir, "root")

	if s.Overlay {
		release, err := cached(img).MountRoot(dir, root)

		return root, release, err
	}

	return root, func() error { return nil }, cached(img).CloneRoot(root)
}

// cached returns the cached image of a run image, which needs only the root for these operations.
func cached(img run.Image) *Image {
	return &Image{Root: img.ID, Config: run.ImageConfig{}}
}
