package node

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
// It copies the bundle to dst.new, checks every file against SHA256SUMS, and then exchanges dst.new and dst atomically.
// A failure leaves dst unchanged. Running VMs keep their open files after the exchange.
func installBundle(src, dst string) error {
	staged := dst + ".new"

	err := os.RemoveAll(staged)
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	want, err := os.ReadFile(filepath.Join(src, sumsFile))
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	installed, err := os.ReadFile(filepath.Join(dst, sumsFile))
	if err == nil && bytes.Equal(installed, want) {
		return nil
	}

	err = stageBundle(src, staged)
	if err != nil {
		return errors.Join(fmt.Errorf("install bundle: %w", err), os.RemoveAll(staged))
	}

	_, err = os.Stat(dst)
	if err == nil {
		// staged now holds the previous bundle.
		err = exchange(staged, dst)
	} else {
		err = os.Rename(staged, dst)
	}

	if err != nil {
		return errors.Join(fmt.Errorf("install bundle: %w", err), os.RemoveAll(staged))
	}

	err = errors.Join(syncDir(filepath.Dir(dst)), os.RemoveAll(staged))
	if err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}

	return nil
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
