// Package image pulls OCI images, unpacks them into a local cache, and prepares per-run roots.
package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// ErrBusy means that a run holds the cache, so Prune cannot remove it.
var ErrBusy = errors.New("the image cache is in use by a running sbxr run")

// lockPoll is how often a waiting lock retries.
const lockPoll = 50 * time.Millisecond

// Cache is the on-disk image cache.
//
// Layout under the cache directory:
//   - images/<algorithm>-<hex>/: one unpacked image per manifest digest, with rootfs/ and config.json.
//   - refs/: one file per image reference and platform that names the manifest digest.
//   - runs/: per-run state that the runner owns.
//   - locks/: one lock file per image reference, held while pulling it.
//   - lock: held shared by each run and exclusive by Prune.
type Cache struct {
	dir string
	own Ownership
}

// Open creates the cache layout in dir when needed and unpacks images with the given ownership strategy.
func Open(dir string, own Ownership) (*Cache, error) {
	for _, sub := range []string{"images", "refs", "runs", "locks"} {
		err := os.MkdirAll(filepath.Join(dir, sub), 0o700)
		if err != nil {
			return nil, fmt.Errorf("create image cache: %w", err)
		}
	}

	return &Cache{dir: dir, own: own}, nil
}

// Hold takes a shared hold on the cache that keeps Prune out until it is closed.
// A run holds it from before Pull until its per-run root is removed.
func (c *Cache) Hold(ctx context.Context) (*os.File, error) {
	f, err := lockFile(ctx, filepath.Join(c.dir, "lock"), unix.LOCK_SH)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// Prune removes every cached image and leftover run state.
// Prune does not wait: it fails with ErrBusy while any run holds the cache.
func (c *Cache) Prune() error {
	f, err := openLock(filepath.Join(c.dir, "lock"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	err = flock(f, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrBusy
	}

	if err != nil {
		return fmt.Errorf("lock image cache: %w", err)
	}

	for _, sub := range []string{"images", "refs", "runs", "locks"} {
		err := os.RemoveAll(filepath.Join(c.dir, sub))
		if err != nil {
			return fmt.Errorf("prune image cache: %w", err)
		}

		err = os.Mkdir(filepath.Join(c.dir, sub), 0o700)
		if err != nil {
			return fmt.Errorf("prune image cache: %w", err)
		}
	}

	return nil
}

// lockFile waits until it holds the flock on path or ctx ends.
func lockFile(ctx context.Context, path string, how int) (*os.File, error) {
	f, err := openLock(path)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()

	for {
		err = flock(f, how|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}

		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()

			return nil, fmt.Errorf("lock %s: %w", path, err)
		}

		select {
		case <-ctx.Done():
			_ = f.Close()

			return nil, fmt.Errorf("wait for lock %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func openLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}

	return f, nil
}

func flock(f *os.File, how int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("lock %s: %w", f.Name(), err)
	}

	var lockErr error

	err = conn.Control(func(fd uintptr) {
		lockErr = unix.Flock(int(fd), how)
	})
	if err != nil {
		return fmt.Errorf("lock %s: %w", f.Name(), err)
	}

	return lockErr //nolint:wrapcheck // Callers wrap the error after checking for EWOULDBLOCK.
}
