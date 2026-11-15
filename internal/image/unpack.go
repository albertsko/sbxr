package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"golang.org/x/sys/unix"
)

// ErrCaseConflict means that an image has paths that differ only by letter case on a case-insensitive file system.
var ErrCaseConflict = errors.New("image has paths that differ only by letter case, which this case-insensitive file system cannot hold")

// OverrideStatXattr is the extended attribute that the libkrun virtio-fs passthrough reads for guest ownership and mode.
const OverrideStatXattr = "user.containers.override_stat"

const (
	opaqueWhiteout = ".wh..wh..opq"
	whiteoutPrefix = ".wh."
	setIDBits      = 0o7777
)

// Ownership selects how image file owners and modes reach the guest.
type Ownership int

const (
	// OwnershipXattr keeps files owned by the current user and stores the image owner and mode in OverrideStatXattr.
	OwnershipXattr Ownership = iota
	// OwnershipChown sets real owners and modes, which needs a user namespace with the image ids mapped.
	OwnershipChown
)

// Unpacker applies image layers to a directory.
type Unpacker struct {
	Ownership Ownership
}

// entryMeta is the owner, group, and mode of one image file.
type entryMeta struct {
	uid, gid int
	typ      uint32
	mode     int64
}

var rootMeta = entryMeta{uid: 0, gid: 0, typ: unix.S_IFDIR, mode: 0o755}

// Unpack applies the layers of img in order to the existing empty directory dst.
func (u Unpacker) Unpack(img v1.Image, dst string) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("read layers: %w", err)
	}

	insensitive, err := caseInsensitive(dst)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open unpack root: %w", err)
	}
	defer func() { _ = root.Close() }()

	l := layerApplier{u: u, root: root, dir: dst, insensitive: insensitive, cases: map[string]string{}}

	err = l.setMeta(".", rootMeta)
	if err != nil {
		return err
	}

	for i, layer := range layers {
		err := l.apply(layer)
		if err != nil {
			return fmt.Errorf("layer %d: %w", i+1, err)
		}
	}

	return nil
}

// layerApplier writes layers into one root directory.
type layerApplier struct {
	u           Unpacker
	root        *os.Root
	dir         string
	insensitive bool
	// cases maps each lowercased path that the image wrote to its spelling, across all layers.
	cases map[string]string
}

func (l layerApplier) apply(layer v1.Layer) error {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("read layer: %w", err)
	}
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	written := map[string]bool{}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}

		name, err := cleanName(hdr.Name)
		if err != nil {
			return err
		}

		// A whiteout names the entry it removes, and an opaque whiteout names its directory.
		dir, base := path.Split(name)
		opaque := base == opaqueWhiteout
		whiteout := !opaque && strings.HasPrefix(base, whiteoutPrefix)

		switch {
		case opaque:
			name = path.Clean(dir)
		case whiteout:
			name = path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))
		}

		// The last component is not followed: an entry replaces a symlink, and a whiteout removes the symlink itself.
		name, err = resolveInRoot(l.root, name, opaque)
		if err != nil {
			return fmt.Errorf("unpack %q: %w", hdr.Name, err)
		}

		if l.insensitive {
			err = l.checkCase(name)
			if err != nil {
				return err
			}
		}

		switch {
		case opaque:
			err = l.clearLower(name, written)
		case whiteout:
			err = l.root.RemoveAll(name)
		default:
			written[name] = true
			err = l.entry(name, hdr, tr)
		}

		if err != nil {
			return fmt.Errorf("unpack %q: %w", hdr.Name, err)
		}
	}
}

// entry writes one tar entry, which replaces any existing entry except a directory over a directory.
func (l layerApplier) entry(name string, hdr *tar.Header, r io.Reader) error {
	if name == "." {
		return l.setMeta(name, metaOf(hdr, unix.S_IFDIR))
	}

	err := l.mkdirParents(path.Dir(name))
	if err != nil {
		return err
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return l.dirEntry(name, hdr)
	case tar.TypeReg:
		return l.fileEntry(name, hdr, r)
	case tar.TypeSymlink:
		err = l.replace(name)
		if err != nil {
			return err
		}

		err = l.root.Symlink(hdr.Linkname, name)
		if err != nil {
			return err //nolint:wrapcheck // The caller adds the entry name.
		}

		return l.setMeta(name, metaOf(hdr, unix.S_IFLNK))
	case tar.TypeLink:
		target, err := cleanName(hdr.Linkname)
		if err != nil {
			return err
		}

		target, err = resolveInRoot(l.root, target, false)
		if err != nil {
			return err
		}

		err = l.replace(name)
		if err != nil {
			return err
		}

		return l.root.Link(target, name) //nolint:wrapcheck // The caller adds the entry name.
	default:
		// The guest mounts its own /dev, so device nodes, fifos, and other special entries are skipped.
		return nil
	}
}

func (l layerApplier) dirEntry(name string, hdr *tar.Header) error {
	fi, err := l.root.Lstat(name)
	if err != nil || !fi.IsDir() {
		err = l.replace(name)
		if err != nil {
			return err
		}

		err = l.root.Mkdir(name, 0o700)
		if err != nil {
			return err //nolint:wrapcheck // The caller adds the entry name.
		}
	}

	return l.setMeta(name, metaOf(hdr, unix.S_IFDIR))
}

func (l layerApplier) fileEntry(name string, hdr *tar.Header, r io.Reader) error {
	err := l.replace(name)
	if err != nil {
		return err
	}

	f, err := l.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	_, err = io.Copy(f, r)
	if err != nil {
		_ = f.Close()

		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	err = f.Close()
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	// Keeping mtimes keeps compiled caches such as Python .pyc files valid.
	err = l.root.Chtimes(name, hdr.ModTime, hdr.ModTime)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	return l.setMeta(name, metaOf(hdr, unix.S_IFREG))
}

// replace removes an existing entry at name.
func (l layerApplier) replace(name string) error {
	err := l.root.RemoveAll(name)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	return nil
}

// mkdirParents creates missing parent directories that the layer does not list.
func (l layerApplier) mkdirParents(dir string) error {
	if dir == "." {
		return nil
	}

	_, err := l.root.Lstat(dir)
	if err == nil {
		return nil
	}

	err = l.mkdirParents(path.Dir(dir))
	if err != nil {
		return err
	}

	err = l.root.Mkdir(dir, 0o700)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	return l.setMeta(dir, rootMeta)
}

// clearLower removes entries under dir that this layer did not write.
func (l layerApplier) clearLower(dir string, written map[string]bool) error {
	entries, err := fs.ReadDir(l.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	for _, e := range entries {
		name := path.Join(dir, e.Name())

		switch {
		case !written[name]:
			err = l.root.RemoveAll(name)
		case e.IsDir():
			err = l.clearLower(name, written)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (l layerApplier) setMeta(name string, m entryMeta) error {
	if l.u.Ownership == OwnershipChown {
		return l.chown(name, m)
	}

	if m.typ != unix.S_IFLNK {
		hostMode := os.FileMode(0o644)
		if m.typ == unix.S_IFDIR || m.mode&0o111 != 0 {
			hostMode = 0o755
		}

		err := l.root.Chmod(name, hostMode)
		if err != nil {
			return err //nolint:wrapcheck // The caller adds the entry name.
		}
	}

	// Linux allows user extended attributes only on regular files and directories.
	if runtime.GOOS == "linux" && m.typ == unix.S_IFLNK {
		return nil
	}

	// The root operations above resolved name inside the root, so the host path stays inside it.
	err := unix.Lsetxattr(filepath.Join(l.dir, name), OverrideStatXattr, overrideStat(m), 0)
	if err != nil {
		return fmt.Errorf("set %s: %w", OverrideStatXattr, err)
	}

	return nil
}

func (l layerApplier) chown(name string, m entryMeta) error {
	err := l.root.Lchown(name, m.uid, m.gid)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the entry name.
	}

	if m.typ == unix.S_IFLNK {
		return nil
	}

	// Chmod runs after Lchown, because chown clears the setuid and setgid bits.
	mode := os.FileMode(m.mode & 0o777)
	if m.mode&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}

	if m.mode&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}

	if m.mode&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}

	return l.root.Chmod(name, mode) //nolint:wrapcheck // The caller adds the entry name.
}

// overrideStat formats the value that libkrun parses as decimal uid, decimal gid, and octal st_mode with file type bits.
func overrideStat(m entryMeta) []byte {
	return fmt.Appendf(nil, "%d:%d:0%o", m.uid, m.gid, int64(m.typ)|m.mode&setIDBits)
}

func metaOf(hdr *tar.Header, typ uint32) entryMeta {
	return entryMeta{uid: hdr.Uid, gid: hdr.Gid, typ: typ, mode: hdr.Mode & setIDBits}
}

// cleanName makes a tar entry name relative to the root and rejects names that leave it.
func cleanName(name string) (string, error) {
	clean := path.Clean(strings.TrimLeft(name, "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the image root", name)
	}

	return clean, nil
}

// checkCase records name and its parents and fails when one differs only by letter case from an existing path of the image.
func (l layerApplier) checkCase(name string) error {
	for p := name; p != "."; p = path.Dir(p) {
		key := strings.ToLower(p)

		prev, ok := l.cases[key]
		if ok && prev == p {
			return nil
		}

		// A removed path frees its name for another spelling.
		if ok {
			_, err := l.root.Lstat(prev)
			if err == nil {
				return fmt.Errorf("%w: %q and %q", ErrCaseConflict, prev, p)
			}
		}

		l.cases[key] = p
	}

	return nil
}

// caseInsensitive probes whether dir treats names that differ only by letter case as the same name.
func caseInsensitive(dir string) (bool, error) {
	probe, err := os.CreateTemp(dir, "case-probe-")
	if err != nil {
		return false, fmt.Errorf("probe file system case sensitivity: %w", err)
	}

	_ = probe.Close()
	defer func() { _ = os.Remove(probe.Name()) }()

	_, err = os.Lstat(filepath.Join(dir, strings.ToUpper(filepath.Base(probe.Name()))))

	return err == nil, nil
}
