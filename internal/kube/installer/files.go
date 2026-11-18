package installer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lock takes an exclusive host lock, so that two installer pods on one node never interleave.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host lock: %w", err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("take host lock: %w", err)
	}

	return func() { _ = f.Close() }, nil // Closing releases the lock.
}

// lockShadow takes the lock that shadow's tools, such as usermod, take before they rewrite path: they link a file
// with their pid to path.lock. A lock whose process is gone is stale and goes, as shadow does.
func lockShadow(path string) (func(), error) {
	lockPath := path + ".lock"
	tmp := path + "." + strconv.Itoa(os.Getpid())

	err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	defer os.Remove(tmp) //nolint:errcheck // The link keeps the lock.

	for range 100 {
		err = os.Link(tmp, lockPath)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}

		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}

		data, _ := os.ReadFile(lockPath)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			_ = os.Remove(lockPath)

			continue
		}

		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("lock %s: another process holds %s", path, lockPath)
}

// writeFile replaces path with data atomically and durably: it syncs the temporary file before the rename and the directory after it.
// A replaced file keeps its mode and owner.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o755) //nolint:gosec // containerd config directories are world-readable.
	if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // containerd configs are world-readable.
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}

	_, err = f.Write(data)

	if st, statErr := os.Stat(path); statErr == nil && err == nil {
		err = f.Chmod(st.Mode().Perm())

		if sys, ok := st.Sys().(*syscall.Stat_t); ok && err == nil {
			err = f.Chown(int(sys.Uid), int(sys.Gid))
		}
	}

	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	return syncDir(dir)
}

// restoreFile puts back the previous content of path, or removes path when there was none.
func restoreFile(path string, previous []byte) error {
	if previous != nil {
		return writeFile(path, previous)
	}

	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}

	err = errors.Join(d.Sync(), d.Close())
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}

	return nil
}

// sumsFile lists the SHA-256 of every regular file in the bundle, in sha256sum format.
const sumsFile = "SHA256SUMS"

// installBundle installs the bundle at src to dst, unless dst already has the same SHA256SUMS.
//
// It copies the bundle to a new directory below retired, checks every file against SHA256SUMS, and then exchanges that
// directory and dst atomically, so the previous bundle lands in the retired directory in the same step. A failure leaves
// dst unchanged, and a partial copy is a retired bundle that no process uses, which removeRetired deletes.
//
// The previous bundle must not go away while it is in use. A running VM holds its guest root by a descriptor
// that follows renames (libkrun's virtio-fs opens the root with O_PATH and looks up every path relative to it), and its
// agent still runs crun from that root for each exec, probe, kill, and delete. Old shims keep their
// binaries and libraries there too. afterExchange is for tests and is nil otherwise.
func installBundle(src, dst, retired string, afterExchange func() error) error {
	want, err := os.ReadFile(filepath.Join(src, sumsFile))
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	installed, err := os.ReadFile(filepath.Join(dst, sumsFile))
	if err == nil && bytes.Equal(installed, want) {
		return nil
	}

	err = os.MkdirAll(retired, 0o755) //nolint:gosec // Like /opt/sbxr.
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	staged, err := os.MkdirTemp(retired, time.Now().UTC().Format("20060102T150405-"))
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	err = errors.Join(os.Remove(staged), stageBundle(src, staged))
	if err != nil {
		return errors.Join(fmt.Errorf("install bundle: %w", err), os.RemoveAll(staged))
	}

	if _, err := os.Stat(dst); err != nil {
		err = os.Rename(staged, dst)
		if err != nil {
			return errors.Join(fmt.Errorf("install bundle: %w", err), os.RemoveAll(staged))
		}

		return syncDir(filepath.Dir(dst))
	}

	// Either both names change or neither does. staged then holds the previous bundle, already retired.
	err = exchange(staged, dst)
	if err != nil {
		return errors.Join(fmt.Errorf("install bundle: %w", err), os.RemoveAll(staged))
	}

	if afterExchange != nil {
		if err := afterExchange(); err != nil {
			return err
		}
	}

	err = errors.Join(syncDir(filepath.Dir(dst)), syncDir(retired))
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	return nil
}

// removeRetired deletes every retired bundle below retired that no process uses, and returns the bundles that stay,
// each with a process that uses it. hostRoot is where the host file system is mounted, and proc is the host's /proc.
func removeRetired(retired, hostRoot, proc string) (map[string]int, error) {
	kept := map[string]int{}

	entries, err := os.ReadDir(retired)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(entries) == 0) {
		return kept, nil
	}

	if err != nil {
		return nil, fmt.Errorf("list retired bundles: %w", err)
	}

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		dirs = append(dirs, filepath.Join(retired, e.Name()))
	}

	users, err := dirUsers(proc, hostRoot, dirs)
	if err != nil {
		return nil, err
	}

	var errs []error

	for _, dir := range dirs {
		if pid, ok := users[dir]; ok {
			kept[filepath.Base(dir)] = pid

			continue
		}

		errs = append(errs, os.RemoveAll(dir))
	}

	return kept, errors.Join(errs...)
}

// dirUsers returns, for each directory in dirs that some process uses, one such process. dirs are paths below hostRoot.
//
// A process uses a directory when its executable, working directory, root, an open file, or a mapped file is in it.
// /proc shows these paths after renames, from the reader's root, so a file of the host shows with or without hostRoot.
// A process that cannot be read, other than one that is gone, counts as using every directory.
func dirUsers(proc, hostRoot string, dirs []string) (map[string]int, error) {
	pids, err := os.ReadDir(proc)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}

	users := map[string]int{}

	note := func(pid int, path string) {
		for _, dir := range dirs {
			for _, d := range []string{dir, strings.TrimPrefix(dir, hostRoot)} {
				if path == d || strings.HasPrefix(path, d+"/") {
					users[dir] = pid
				}
			}
		}
	}

	unreadable := func(pid int, err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
			for _, dir := range dirs {
				users[dir] = pid
			}
		}
	}

	for _, p := range pids {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}

		scanProcess(filepath.Join(proc, p.Name()), func(path string) { note(pid, path) }, func(err error) { unreadable(pid, err) })
	}

	return users, nil
}

// scanProcess reports every file path that the process at dir uses, and every error other than a missing link.
func scanProcess(dir string, path func(string), fail func(error)) {
	for _, link := range []string{"exe", "cwd", "root"} {
		target, err := os.Readlink(filepath.Join(dir, link))
		if err == nil {
			path(target)
		} else {
			// Kernel threads have no executable, and a process that just exited has no links.
			fail(err)
		}
	}

	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	fail(err)

	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
		if err == nil {
			path(target)
		} else {
			fail(err)
		}
	}

	maps, err := os.ReadFile(filepath.Join(dir, "maps"))
	fail(err)

	for line := range strings.Lines(string(maps)) {
		// The path is the sixth field. It may contain spaces, and it ends in " (deleted)" for removed files.
		if f := strings.Fields(line); len(f) >= 6 {
			path(strings.Join(f[5:], " "))
		}
	}
}

// stageBundle copies src to dst and fails unless every file matches SHA256SUMS and every listed file exists.
func stageBundle(src, dst string) error {
	sums, err := readSums(filepath.Join(src, sumsFile))
	if err != nil {
		return err
	}

	err = filepath.WalkDir(src, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)

		switch {
		case e.IsDir():
			return os.MkdirAll(target, 0o755) //nolint:gosec // The bundle is world-readable, like /usr.
		case e.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err //nolint:wrapcheck // WalkDir callers wrap.
			}

			return os.Symlink(link, target) //nolint:gosec // The staging directory is new and only the installer writes it.
		case rel == sumsFile:
			_, err := copyFile(path, target)

			return err
		}

		want, ok := sums[rel]
		if !ok {
			return fmt.Errorf("%s is not in %s", rel, sumsFile)
		}

		delete(sums, rel)

		got, err := copyFile(path, target)
		if err == nil && got != want {
			err = fmt.Errorf("%s has SHA-256 %s, want %s", rel, got, want)
		}

		return err
	})
	if err != nil {
		return err //nolint:wrapcheck // installBundle wraps.
	}

	if len(sums) > 0 {
		return fmt.Errorf("missing files %v", slices.Sorted(maps.Keys(sums)))
	}

	return nil
}

func readSums(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}

	sums := map[string]string{}

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			return nil, fmt.Errorf("checksums: bad line %q", sc.Text())
		}

		sums[strings.TrimPrefix(name, "./")] = sum
	}

	if len(sums) == 0 {
		return nil, errors.New("checksums: no files")
	}

	return sums, nil
}

// copyFile copies src to dst, syncs it, and returns the SHA-256 of the content.
func copyFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close() //nolint:errcheck // Read-only file.

	st, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dst, err)
	}

	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)

	err = errors.Join(err, out.Sync(), out.Close())
	if err != nil {
		return "", fmt.Errorf("copy %s: %w", src, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// linkShim points link at the installed shim. It replaces the link atomically.
func linkShim(link string) error {
	tmp := link + ".tmp"
	_ = os.Remove(tmp)

	err := os.MkdirAll(filepath.Dir(link), 0o755) //nolint:gosec // A bin directory.
	if err == nil {
		err = os.Symlink(shimPath, tmp)
	}

	if err == nil {
		err = os.Rename(tmp, link)
	}

	if err != nil {
		return fmt.Errorf("link the shim at %s: %w", link, err)
	}

	return nil
}

// unlinkShim removes link when it points at the installed shim.
func unlinkShim(link string) error {
	target, err := os.Readlink(link)
	if err != nil || target != shimPath {
		return nil //nolint:nilerr // Not ours, or gone.
	}

	err = os.Remove(link)
	if err != nil {
		return fmt.Errorf("remove %s: %w", link, err)
	}

	return nil
}

// anyRetired returns one of the kept retired bundles, the first by name.
func anyRetired(kept map[string]int) (string, int, bool) {
	names := slices.Sorted(maps.Keys(kept))
	if len(names) == 0 {
		return "", 0, false
	}

	return names[0], kept[names[0]], true
}
