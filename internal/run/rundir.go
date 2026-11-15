package run

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Run state in the runs directory:
//   - <id>.lock: the run lock. It appears only already locked, and the run (and its VM process) holds it until <id> is gone.
//   - <id>: the run directory, created after <id>.lock.
//   - .new-*: a lock file that a run is about to lock and rename to <id>.lock.
//
// So a run directory whose lock is free or missing belongs to a run whose processes died.
// A free .new-* file is stale once it is older than newLockAge, because a run locks and renames it at once.
// The age comes from the file mtime. If a run stalls longer than that between creating and locking the file,
// or the clock jumps, a sweep can remove it. The run then fails to rename the file and stops with an error. No run state is lost.
const (
	lockSuffix = ".lock"
	newPrefix  = ".new-"
	newLockAge = time.Minute
)

// newRunDir removes stale run state in runsDir, then creates a locked run directory.
// lock is the held run lock. unlock releases and removes it. Call unlock after the directory is removed.
func newRunDir(runsDir string, log *slog.Logger) (dir string, lock *os.File, unlock func(), err error) {
	err = os.MkdirAll(runsDir, 0o700)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create runs directory: %w", err)
	}

	removeStale(runsDir, log)

	lock, lockPath, err := newLock(runsDir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create run lock: %w", err)
	}

	unlock = func() {
		_ = os.Remove(lockPath) // A leftover lock file is removed by the next run.
		_ = lock.Close()        // Closing releases the flock.
	}

	dir = strings.TrimSuffix(lockPath, lockSuffix)

	err = os.Mkdir(dir, 0o700)
	if err != nil {
		unlock()

		return "", nil, nil, fmt.Errorf("create run directory: %w", err)
	}

	return dir, lock, unlock, nil
}

// newLock creates a lock file under a name that the sweep leaves alone, locks it, and renames it to <id>.lock.
// It returns the lock and its path.
func newLock(runsDir string) (*os.File, string, error) {
	f, err := os.CreateTemp(runsDir, newPrefix+"*")
	if err != nil {
		return nil, "", err //nolint:wrapcheck // newRunDir wraps the error.
	}

	name := filepath.Join(runsDir, strings.TrimPrefix(filepath.Base(f.Name()), newPrefix)+lockSuffix)

	err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
	if err == nil {
		err = os.Rename(f.Name(), name)
	}

	if err != nil {
		_ = os.Remove(f.Name())
		_ = f.Close()

		return nil, "", err //nolint:wrapcheck // newRunDir wraps the error.
	}

	return f, name, nil
}

// removeStale removes the run directories and lock files of runs whose lock is not held.
func removeStale(runsDir string, log *slog.Logger) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		var dir, lockPath string

		switch {
		case strings.HasPrefix(e.Name(), newPrefix):
			if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < newLockAge {
				continue
			}

			lockPath = filepath.Join(runsDir, e.Name())
		case e.IsDir():
			dir = filepath.Join(runsDir, e.Name())
			lockPath = dir + lockSuffix
		case strings.HasSuffix(e.Name(), lockSuffix):
			// A lock without its directory is stale only when free, which tryLock checks.
			lockPath = filepath.Join(runsDir, e.Name())
		default:
			continue
		}

		lock, held := tryLock(lockPath)
		if held {
			continue
		}

		log.Debug("remove stale run state", "path", lockPath)

		if dir != "" {
			_ = os.RemoveAll(dir) // A failure leaves the state for the next run.
		}

		_ = os.Remove(lockPath) // See above.
		_ = lock.Close()        // Closing a lock file fails only for a closed file.
	}
}

// tryLock takes the lock at path without waiting. held reports that a live run holds it.
// A missing lock file counts as free, because a lock appears before its directory.
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
