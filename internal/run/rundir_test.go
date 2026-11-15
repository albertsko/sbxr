package run

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestNewRunDirRemovesOnlyStaleRuns(t *testing.T) {
	t.Parallel()

	runsDir := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	active, _, unlockActive, err := newRunDir(runsDir, log)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockActive()

	// A run whose parent died after it created its lock and directory leaves both, with the lock free.
	crashed := filepath.Join(runsDir, "crashed")
	mustWrite(t, filepath.Join(crashed, "root", "file"))
	mustWrite(t, crashed+lockSuffix)

	// A directory without a lock file is stale too, because a run creates the lock first.
	unlocked := filepath.Join(runsDir, "unlocked")
	mustWrite(t, filepath.Join(unlocked, "file"))

	// A lone free lock file is stale, and so is a free lock file that a run never renamed.
	mustWrite(t, filepath.Join(runsDir, "lone"+lockSuffix))
	crashedNew := filepath.Join(runsDir, newPrefix+"crashed")
	mustWrite(t, crashedNew)

	old := time.Now().Add(-2 * newLockAge)

	err = os.Chtimes(crashedNew, old, old)
	if err != nil {
		t.Fatal(err)
	}

	// A free but fresh new lock file may belong to a run that is about to lock it.
	fresh := filepath.Join(runsDir, newPrefix+"fresh")
	mustWrite(t, fresh)

	dir, _, unlock, err := newRunDir(runsDir, log)
	if err != nil {
		t.Fatal(err)
	}

	got := entries(t, runsDir)
	want := []string{filepath.Base(fresh), filepath.Base(active), filepath.Base(active) + lockSuffix, filepath.Base(dir), filepath.Base(dir) + lockSuffix}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("runs directory = %q, want only the two active runs %q", got, want)
	}

	err = os.Remove(dir)
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	if got := entries(t, runsDir); len(got) != 3 {
		t.Errorf("runs directory after unlock = %q, want only the first run and the fresh file", got)
	}
}

// TestNewRunDirUnderConcurrentSweeps checks that no run loses its directory or lock to the sweep of another run.
func TestNewRunDirUnderConcurrentSweeps(t *testing.T) {
	t.Parallel()

	runsDir := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	var wg sync.WaitGroup

	errs := make(chan error, 32*20)

	for range 32 {
		wg.Go(func() {
			for range 20 {
				dir, lock, unlock, err := newRunDir(runsDir, log)
				if err != nil {
					errs <- err

					return
				}

				// The run state must survive the sweeps that other runs make meanwhile.
				removeStale(runsDir, log)

				_, dirErr := os.Stat(dir)
				_, lockErr := os.Stat(dir + lockSuffix)

				if dirErr != nil || lockErr != nil || lock == nil {
					errs <- fmt.Errorf("run state of %s is gone: %w", dir, errors.Join(dirErr, lockErr))
				}

				_ = os.Remove(dir)

				unlock()
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	if got := entries(t, runsDir); len(got) != 0 {
		t.Errorf("runs directory = %q, want empty", got)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()

	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.Name())
	}

	return names
}
