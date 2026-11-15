package run

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestNewRunDirRemovesOnlyStaleRuns(t *testing.T) {
	t.Parallel()

	runsDir := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	active, unlockActive, err := newRunDir(runsDir, log)
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

	// A lone free lock file is stale.
	mustWrite(t, filepath.Join(runsDir, "lone"+lockSuffix))

	dir, unlock, err := newRunDir(runsDir, log)
	if err != nil {
		t.Fatal(err)
	}

	got := entries(t, runsDir)
	want := []string{filepath.Base(active), filepath.Base(active) + lockSuffix, filepath.Base(dir), filepath.Base(dir) + lockSuffix}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("runs directory = %q, want only the two active runs %q", got, want)
	}

	err = os.Remove(dir)
	if err != nil {
		t.Fatal(err)
	}

	unlock()

	if got := entries(t, runsDir); len(got) != 2 {
		t.Errorf("runs directory after unlock = %q, want only the first run", got)
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
