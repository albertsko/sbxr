package image_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

func TestPullReturnsImageConfig(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	cfg := v1.Config{
		Entrypoint: []string{"/bin/entry"},
		Cmd:        []string{"serve", "--port", "80"},
		Env:        []string{"PATH=/usr/bin:/bin", "MODE=test"},
		WorkingDir: "/srv",
		User:       "alice",
	}
	ref := reg.push(t, "app", testImage(t, runtime.GOARCH, cfg, layer(t, file("srv/index", "ok"))))
	c, _ := openCache(t)

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := image.Config{Entrypoint: cfg.Entrypoint, Cmd: cfg.Cmd, Env: cfg.Env, WorkingDir: cfg.WorkingDir, User: cfg.User}
	if !reflect.DeepEqual(img.Config, want) {
		t.Errorf("Config = %+v, want %+v", img.Config, want)
	}

	if got := readFile(t, at(img.Root, "srv/index")); got != "ok" {
		t.Errorf("srv/index = %q, want ok", got)
	}
}

func TestPullReportsProgress(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	ref := reg.push(t, "app", hostImage(t, layer(t, file("a", "aaaa")), layer(t, file("b", "bb"))))
	c, _ := openCache(t)

	var got []image.Progress

	_, err := c.Pull(t.Context(), ref, image.PullMissing, func(p image.Progress) { got = append(got, p) })
	if err != nil {
		t.Fatal(err)
	}

	if len(got) < 3 {
		t.Fatalf("progress %+v, want updates for both layers and a final one", got)
	}

	for i := 1; i < len(got); i++ {
		if got[i].Bytes < got[i-1].Bytes || got[i].Layer < got[i-1].Layer || got[i].Layers != 2 {
			t.Errorf("progress %+v after %+v, want growing bytes and layers of 2", got[i], got[i-1])
		}
	}

	if last := got[len(got)-1]; !last.Done || last.Layer != 2 || slices.ContainsFunc(got[:len(got)-1], func(p image.Progress) bool { return p.Done }) {
		t.Errorf("progress %+v, want only the last update done at layer 2", got)
	}

	// A cached image needs no download, so it reports no progress.
	got = nil

	_, err = c.Pull(t.Context(), ref, image.PullMissing, func(p image.Progress) { got = append(got, p) })
	if err != nil || len(got) != 0 {
		t.Errorf("cached Pull = %v, progress %+v; want no error and no progress", err, got)
	}
}

func TestPullMissingImage(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	c, dir := openCache(t)
	ref := reg.ref("missing:latest")

	_, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("Pull error = %v, want ErrNotFound", err)
	}

	assertNoCacheEntry(t, dir)
}

func TestPullFailedUnpackLeavesNoCacheEntry(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	ref := reg.push(t, "bad", hostImage(t, layer(t, file("ok", "x"), file("../escaped", "x"))))
	c, dir := openCache(t)

	if _, err := c.Pull(t.Context(), ref, image.PullMissing, nil); err == nil {
		t.Fatal("Pull succeeded, want an error")
	}

	assertNoCacheEntry(t, dir)
}

func TestPullSelectsHostPlatform(t *testing.T) {
	t.Parallel()

	other := "s390x"
	reg := newRegistry(t)
	idx := mutate.AppendManifests(empty.Index,
		addendum(t, other, "other"),
		addendum(t, runtime.GOARCH, "host"),
	)
	ref := reg.pushIndex(t, "multi", idx)
	c, _ := openCache(t)

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(img.Root, "arch")); got != "host" {
		t.Errorf("arch = %q, want the host variant", got)
	}
}

func TestPullRejectsALayerWithTheWrongDigest(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	good := layer(t, file("a", "good"))
	ref := reg.push(t, "app", hostImage(t, good))
	reg.swap(t, good, layer(t, file("a", "evil")))
	c, dir := openCache(t)

	_, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Pull error = %v, want a digest mismatch", err)
	}

	assertNoCacheEntry(t, dir)
}

func TestPullWithoutHostPlatform(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	idx := mutate.AppendManifests(empty.Index, addendum(t, "s390x", "a"), addendum(t, "riscv64", "b"))
	tests := map[string]struct {
		ref  string
		want string
	}{
		"index":  {ref: reg.pushIndex(t, "multi", idx), want: "linux/riscv64, linux/s390x"},
		"single": {ref: reg.push(t, "single", testImage(t, "s390x", v1.Config{})), want: "linux/s390x"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, dir := openCache(t)

			_, err := c.Pull(t.Context(), tt.ref, image.PullMissing, nil)
			if !errors.Is(err, image.ErrNoPlatform) {
				t.Fatalf("Pull error = %v, want ErrNoPlatform", err)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not list %s", err, tt.want)
			}

			assertNoCacheEntry(t, dir)
		})
	}
}

func TestPullPolicy(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	ref := reg.push(t, "app", hostImage(t, layer(t, file("v", "1"))))
	c, _ := openCache(t)

	if _, err := c.Pull(t.Context(), ref, image.PullNever, nil); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("Pull never on an empty cache: error = %v, want ErrNotFound", err)
	}

	if _, err := c.Pull(t.Context(), ref, image.PullMissing, nil); err != nil {
		t.Fatal(err)
	}

	downloads := reg.blobGets()

	if _, err := c.Pull(t.Context(), ref, image.PullMissing, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Pull(t.Context(), ref, image.PullAlways, nil); err != nil {
		t.Fatal(err)
	}

	if got := reg.blobGets(); got != downloads {
		t.Errorf("blob downloads after cached pulls = %d, want %d", got, downloads)
	}

	reg.push(t, "app", hostImage(t, layer(t, file("v", "2"))))

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(img.Root, "v")); got != "1" {
		t.Errorf("v after Pull missing = %q, want the cached 1", got)
	}

	img, err = c.Pull(t.Context(), ref, image.PullAlways, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(img.Root, "v")); got != "2" {
		t.Errorf("v after Pull always = %q, want the updated 2", got)
	}

	reg.srv.Close()

	if _, err := c.Pull(t.Context(), ref, image.PullNever, nil); err != nil {
		t.Errorf("Pull never with the registry down: %v", err)
	}
}

func TestPullConcurrentDownloadsOnce(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	files := make([]entry, 0, 50)

	for i := range cap(files) {
		files = append(files, file(fmt.Sprintf("data/f%02d", i), strings.Repeat("y", 4096)))
	}

	ref := reg.push(t, "big", hostImage(t, layer(t, files...), layer(t, file("last", "done"))))
	c, _ := openCache(t)

	const runs = 8

	var wg sync.WaitGroup

	errs := make(chan error, runs)
	for range runs {
		wg.Go(func() {
			img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
			if err != nil {
				errs <- err

				return
			}

			// A partial root would miss the last layer or some of the data files.
			data, err := os.ReadDir(filepath.Join(img.Root, "data"))
			if err == nil && (len(data) != len(files) || !exists(filepath.Join(img.Root, "last"))) {
				err = errors.New("partial root")
			}

			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	// One config blob and two layer blobs.
	if got := reg.blobGets(); got != 3 {
		t.Errorf("blob downloads = %d, want 3", got)
	}
}

func TestPruneRefusesWhileHeld(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	ref := reg.push(t, "app", hostImage(t, layer(t, file("f", "x"))))
	c, _ := openCache(t)

	hold, err := c.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	img, err := c.Pull(t.Context(), ref, image.PullMissing, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Prune(); !errors.Is(err, image.ErrBusy) {
		t.Fatalf("Prune with a hold: error = %v, want ErrBusy", err)
	}

	if !exists(img.Root) {
		t.Fatal("Prune removed an image while a run held the cache")
	}

	hold.Close()

	if err := c.Prune(); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Pull(t.Context(), ref, image.PullNever, nil); !errors.Is(err, image.ErrNotFound) {
		t.Errorf("Pull never after Prune: error = %v, want ErrNotFound", err)
	}
}

func addendum(t *testing.T, arch, marker string) mutate.IndexAddendum {
	t.Helper()

	return mutate.IndexAddendum{
		Add:      testImage(t, arch, v1.Config{}, layer(t, file("arch", marker))),
		Platform: &v1.Platform{OS: "linux", Architecture: arch},
	}
}

func assertNoCacheEntry(t *testing.T, dir string) {
	t.Helper()

	for _, sub := range []string{"images", "refs"} {
		if names := dirEntries(t, filepath.Join(dir, sub)); len(names) != 0 {
			t.Errorf("%s has entries %v, want none", sub, names)
		}
	}
}
