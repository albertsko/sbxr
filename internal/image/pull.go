package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/sys/unix"
)

var (
	// ErrNotFound means that the registry does not have the image or the cache does not have it under PullNever.
	ErrNotFound = errors.New("image not found")
	// ErrNoPlatform means that the image has no variant for the host architecture.
	ErrNoPlatform = errors.New("image has no variant for the host platform")
)

// PullPolicy decides when Pull contacts the registry.
type PullPolicy string

const (
	// PullMissing pulls only when the cache does not have the image.
	PullMissing PullPolicy = "missing"
	// PullAlways checks the registry and pulls when the image changed.
	PullAlways PullPolicy = "always"
	// PullNever uses only the cache.
	PullNever PullPolicy = "never"
)

// Image is an unpacked image in the cache. Its files must not change.
type Image struct {
	// Digest is the manifest digest of the platform image.
	Digest string
	// Root is the cached root directory.
	Root string
	// Config is the image run configuration.
	Config Config
}

// Config is the part of the image configuration that decides what runs.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
}

// hostPlatform is the only guest platform, because sbxr does not emulate other architectures.
const hostPlatform = "linux/" + runtime.GOARCH

// Pull returns the cached image for ref, and downloads and unpacks it first when policy requires.
// Concurrent pulls of one reference download it once.
// The caller holds Hold while it pulls and uses the image.
func (c *Cache) Pull(ctx context.Context, ref string, policy PullPolicy) (*Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", ref, err)
	}

	sum := sha256.Sum256([]byte(r.Name() + "\n" + hostPlatform))
	key := hex.EncodeToString(sum[:])

	if policy != PullAlways {
		img, err := c.lookup(key)
		if err != nil || img != nil {
			return img, err
		}

		if policy == PullNever {
			return nil, fmt.Errorf("image %q: %w in the cache and the pull policy is never", ref, ErrNotFound)
		}
	}

	lock, err := lockFile(ctx, filepath.Join(c.dir, "locks", key), unix.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()

	if policy == PullMissing {
		img, err := c.lookup(key)
		if err != nil || img != nil {
			return img, err
		}
	}

	img, err := c.fetch(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("pull image %q: %w", ref, err)
	}

	cached, err := c.store(key, img)
	if err != nil {
		return nil, fmt.Errorf("pull image %q: %w", ref, err)
	}

	return cached, nil
}

// lookup returns the cached image for a reference key, or nil when the cache does not have it.
func (c *Cache) lookup(key string) (*Image, error) {
	digest, err := os.ReadFile(filepath.Join(c.dir, "refs", key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // A missing entry is not an error.
	}

	if err != nil {
		return nil, fmt.Errorf("read image cache: %w", err)
	}

	img, err := c.load(string(digest))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // A dangling reference is a missing entry.
	}

	return img, err
}

func (c *Cache) load(digest string) (*Image, error) {
	dir := c.imageDir(digest)

	f, err := os.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read image cache: %w", err)
	}
	defer func() { _ = f.Close() }()

	cf, err := v1.ParseConfigFile(f)
	if err != nil {
		return nil, fmt.Errorf("read image config %s: %w", digest, err)
	}

	cfg := Config{
		Entrypoint: cf.Config.Entrypoint,
		Cmd:        cf.Config.Cmd,
		Env:        cf.Config.Env,
		WorkingDir: cf.Config.WorkingDir,
		User:       cf.Config.User,
	}

	return &Image{Digest: digest, Root: filepath.Join(dir, "rootfs"), Config: cfg}, nil
}

func (c *Cache) imageDir(digest string) string {
	return filepath.Join(c.dir, "images", strings.Replace(digest, ":", "-", 1))
}

// fetch resolves r to the image for the host platform.
// A single-platform image is checked when it is unpacked, so a cached image needs no config download.
func (c *Cache) fetch(ctx context.Context, r name.Reference) (v1.Image, error) {
	desc, err := remote.Get(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, registryError(err)
	}

	if !desc.MediaType.IsIndex() {
		img, err := desc.Image()
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}

		return img, nil
	}

	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("read image index: %w", err)
	}

	manifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read image index: %w", err)
	}

	var available []string

	for _, m := range manifest.Manifests {
		p := m.Platform
		if p == nil || p.OS == "unknown" {
			continue
		}

		if p.OS+"/"+p.Architecture == hostPlatform {
			img, err := idx.Image(m.Digest)
			if err != nil {
				return nil, fmt.Errorf("read image: %w", err)
			}

			return img, nil
		}

		available = append(available, p.String())
	}

	return nil, platformError(available)
}

// store unpacks img into the cache under a temporary name, renames it into place, and records the reference.
func (c *Cache) store(key string, img v1.Image) (*Image, error) {
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("read image digest: %w", err)
	}

	dir := c.imageDir(digest.String())

	_, err = os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		err = c.unpack(img, dir)
	}

	if err != nil {
		return nil, err
	}

	err = writeAtomic(filepath.Join(c.dir, "refs", key), []byte(digest.String()))
	if err != nil {
		return nil, err
	}

	return c.load(digest.String())
}

func (c *Cache) unpack(img v1.Image, dir string) error {
	cf, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}

	if cf.OS+"/"+cf.Architecture != hostPlatform {
		return platformError([]string{cf.OS + "/" + cf.Architecture})
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-")
	if err != nil {
		return fmt.Errorf("create image cache entry: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	cfg, err := img.RawConfigFile()
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}

	err = os.WriteFile(filepath.Join(tmp, "config.json"), cfg, 0o600)
	if err != nil {
		return fmt.Errorf("write image config: %w", err)
	}

	err = os.Mkdir(filepath.Join(tmp, "rootfs"), 0o700)
	if err != nil {
		return fmt.Errorf("create image root: %w", err)
	}

	err = c.unpacker.Unpack(img, filepath.Join(tmp, "rootfs"))
	if err != nil {
		return err
	}

	groups, err := hardlinkGroups(filepath.Join(tmp, "rootfs"))
	if err != nil {
		return err
	}

	err = writeHardlinks(tmp, groups)
	if err != nil {
		return err
	}

	err = os.Rename(tmp, dir)
	if err == nil {
		return nil
	}

	// Another reference to the same digest may have stored it first.
	_, statErr := os.Stat(dir)
	if statErr == nil {
		return nil
	}

	return fmt.Errorf("store image: %w", err)
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return fmt.Errorf("write image cache: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	_, err = f.Write(data)
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("write image cache: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("write image cache: %w", err)
	}

	err = os.Rename(f.Name(), path)
	if err != nil {
		return fmt.Errorf("write image cache: %w", err)
	}

	return nil
}

func registryError(err error) error {
	terr, ok := errors.AsType[*transport.Error](err)
	if !ok {
		return fmt.Errorf("contact registry: %w", err)
	}

	for _, d := range terr.Errors {
		switch d.Code { //nolint:exhaustive // Other codes keep the registry message.
		case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode:
			return fmt.Errorf("%w in the registry", ErrNotFound)
		case transport.UnauthorizedErrorCode, transport.DeniedErrorCode:
			return fmt.Errorf("%w, or the registry requires authentication", ErrNotFound)
		}
	}

	if terr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w in the registry", ErrNotFound)
	}

	return fmt.Errorf("contact registry: %w", err)
}

func platformError(available []string) error {
	slices.Sort(available)

	list := strings.Join(available, ", ")
	if list == "" {
		list = "none"
	}

	return fmt.Errorf("%w %s, available platforms: %s", ErrNoPlatform, hostPlatform, list)
}
