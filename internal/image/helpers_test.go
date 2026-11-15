package image_test

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/sys/unix"
)

// Paths that several tests use.
const (
	su     = "bin/su"
	tmpDir = "tmp"
)

// entry is one tar entry of a test layer.
type entry struct {
	name     string
	typ      byte
	body     string
	link     string
	mode     int64
	uid, gid int
}

func file(name, body string) entry {
	return entry{name: name, typ: tar.TypeReg, body: body, mode: 0o644}
}

func dir(name string) entry {
	return entry{name: name, typ: tar.TypeDir, mode: 0o755}
}

func layer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Linkname: e.link,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Size:     int64(len(e.body)),
			ModTime:  time.Unix(1700000000, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return l
}

// testImage builds an image for arch with the given run config and layers.
func testImage(t *testing.T, arch string, cfg v1.Config, layers ...v1.Layer) v1.Image {
	t.Helper()

	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: arch, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}

	img, err = mutate.AppendLayers(img, layers...)
	if err != nil {
		t.Fatal(err)
	}

	return img
}

func hostImage(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()

	return testImage(t, runtime.GOARCH, v1.Config{}, layers...)
}

// testRegistry is an in-process registry on 127.0.0.1 that counts blob downloads.
type testRegistry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	blobs int
}

func newRegistry(t *testing.T) *testRegistry {
	t.Helper()

	r := &testRegistry{}
	h := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/blobs/") {
			r.mu.Lock()
			r.blobs++
			r.mu.Unlock()
		}

		h.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)

	return r
}

func (r *testRegistry) ref(repo string) string {
	return strings.TrimPrefix(r.srv.URL, "http://") + "/" + repo
}

func (r *testRegistry) push(t *testing.T, repo string, img v1.Image) string {
	t.Helper()

	ref := r.ref(repo)
	if err := remote.Write(mustRef(t, ref), img); err != nil {
		t.Fatal(err)
	}

	return ref
}

func (r *testRegistry) pushIndex(t *testing.T, repo string, idx v1.ImageIndex) string {
	t.Helper()

	ref := r.ref(repo)
	if err := remote.WriteIndex(mustRef(t, ref), idx); err != nil {
		t.Fatal(err)
	}

	return ref
}

func (r *testRegistry) blobGets() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.blobs
}

func mustRef(t *testing.T, ref string) name.Reference {
	t.Helper()

	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func openCache(t *testing.T) (*image.Cache, string) {
	t.Helper()

	dir := t.TempDir()

	c, err := image.Open(dir, image.OwnershipXattr)
	if err != nil {
		t.Fatal(err)
	}

	return c, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func overrideStat(t *testing.T, path string) string {
	t.Helper()

	buf := make([]byte, 64)

	n, err := unix.Lgetxattr(path, image.OverrideStatXattr, buf)
	if err != nil {
		t.Fatalf("read %s of %s: %v", image.OverrideStatXattr, path, err)
	}

	return string(buf[:n])
}

func dirEntries(t *testing.T, path string) []string {
	t.Helper()

	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

func unpack(t *testing.T, img v1.Image) (string, error) {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	err := image.Unpacker{Ownership: image.OwnershipXattr}.Unpack(img, dst)
	if err != nil {
		return dst, fmt.Errorf("unpack: %w", err)
	}

	return dst, nil
}

// at joins a slash-separated path inside root.
func at(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(name))
}
