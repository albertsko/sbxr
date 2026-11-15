package run

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// lockSuffix names the lock file next to each run directory.
// A run holds an exclusive flock on it from before the directory exists until the directory is gone,
// so a directory whose lock is free or missing belongs to a run whose parent died.
const lockSuffix = ".lock"

// newRunDir removes stale run directories in runsDir, then creates a locked run directory.
// unlock releases and removes the lock. Call it after the directory is removed.
func newRunDir(runsDir string, log *slog.Logger) (dir string, unlock func(), err error) {
	err = os.MkdirAll(runsDir, 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("create runs directory: %w", err)
	}

	removeStale(runsDir, log)

	lock, err := os.CreateTemp(runsDir, "*"+lockSuffix)
	if err != nil {
		return "", nil, fmt.Errorf("create run lock: %w", err)
	}

	unlock = func() {
		_ = os.Remove(lock.Name()) // A leftover lock file is removed by the next run.
		_ = lock.Close()           // Closing releases the flock.
	}

	err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		dir = strings.TrimSuffix(lock.Name(), lockSuffix)
		err = os.Mkdir(dir, 0o700)
	}

	if err != nil {
		unlock()

		return "", nil, fmt.Errorf("create run directory: %w", err)
	}

	return dir, unlock, nil
}

// removeStale removes the run directories and lock files of runs whose lock is not held.
func removeStale(runsDir string, log *slog.Logger) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		name := filepath.Join(runsDir, strings.TrimSuffix(e.Name(), lockSuffix))
		if !e.IsDir() && !strings.HasSuffix(e.Name(), lockSuffix) {
			continue
		}

		if !e.IsDir() {
			// A lock file without its directory is either stale or a run that is about to create the directory.
			if _, err := os.Lstat(name); err == nil {
				continue
			}
		}

		lock, held := tryLock(name + lockSuffix)
		if held {
			continue
		}

		log.Debug("remove stale run state", "dir", name)

		_ = os.RemoveAll(name)           // A failure leaves the state for the next run.
		_ = os.Remove(name + lockSuffix) // See above.
		_ = lock.Close()                 // Closing a lock file fails only for a closed file.
	}
}

// tryLock takes the lock at path without waiting. held reports that another run holds it.
// A missing lock file counts as free, because a run creates its lock before its directory.
func tryLock(path string) (lock *os.File, held bool) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}

	if err != nil {
		return nil, true
	}

	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = f.Close()

		return nil, true
	}

	return f, false
}
