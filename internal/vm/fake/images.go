package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/spec"
)

// ErrImageNotFound means that the image directory has no image for the reference.
var ErrImageNotFound = errors.New("image not found")

// Images serves images from Dir. Each image is a directory named after the escaped reference,
// with the root file system in rootfs/ and the image config in config.json.
type Images struct {
	Dir string
}

// Pull loads the image config. The fake store never downloads, so the policy has no effect.
func (s Images) Pull(_ context.Context, ref string, _ spec.Pull) (run.Image, error) {
	dir := imageDir(s.Dir, ref)

	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return run.Image{}, ErrImageNotFound
	}

	if err != nil {
		return run.Image{}, fmt.Errorf("read image config: %w", err)
	}

	var cfg run.ImageConfig

	err = json.Unmarshal(data, &cfg)
	if err != nil {
		return run.Image{}, fmt.Errorf("decode image config: %w", err)
	}

	return run.Image{ID: dir, Config: cfg}, nil
}

// NewRoot copies the image root file system into dir/root.
func (s Images) NewRoot(_ context.Context, img run.Image, dir string) (string, error) {
	root := filepath.Join(dir, "root")

	err := os.CopyFS(root, os.DirFS(filepath.Join(img.ID, "rootfs")))
	if err != nil {
		return "", fmt.Errorf("copy image root: %w", err)
	}

	return root, nil
}

// WriteImage adds a busybox-like image for ref to dir.
// The root has the fake busybox in /bin with one symlink per applet, /etc/passwd, /root, and /tmp.
func WriteImage(dir, ref string, cfg run.ImageConfig) error {
	image := imageDir(dir, ref)
	rootfs := filepath.Join(image, "rootfs")

	for _, d := range []string{"bin", "etc", "root", "tmp"} {
		err := os.MkdirAll(filepath.Join(rootfs, d), 0o755) //nolint:gosec // Image directories are world-readable.
		if err != nil {
			return fmt.Errorf("create image: %w", err)
		}
	}

	files := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "bin/busybox", data: Marker, mode: 0o755},
		{name: "etc/passwd", data: "root:x:0:0:root:/root:/bin/sh\n", mode: 0o644},
	}

	for _, f := range files {
		err := os.WriteFile(filepath.Join(rootfs, f.name), []byte(f.data), f.mode)
		if err != nil {
			return fmt.Errorf("create image: %w", err)
		}
	}

	for _, applet := range Applets {
		err := os.Symlink("busybox", filepath.Join(rootfs, "bin", applet))
		if err != nil {
			return fmt.Errorf("create image: %w", err)
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode image config: %w", err)
	}

	err = os.WriteFile(filepath.Join(image, "config.json"), data, 0o600)
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}

	return nil
}

func imageDir(dir, ref string) string {
	return filepath.Join(dir, url.PathEscape(ref))
}
