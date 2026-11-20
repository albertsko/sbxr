package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Files that the test layer adds to every scenario image.
const (
	// SetuidFile is owned by 0:42 with mode 4755, and LinkFile is a hardlink to it.
	SetuidFile = "/data/setuid"
	LinkFile   = "/data/link"
	// HijackFile is a guest init config that runs `echo hijacked`.
	HijackFile = "/data/hijack.json"
)

// serveImages serves the scenario images, built on base, from a registry on 127.0.0.1 until stop.
// It returns the reference of each scenario image name. MissingImage maps to a reference that the registry lacks.
// base needs busybox applets in /bin and an /etc/passwd with nobody (65534:65534).
func serveImages(ctx context.Context, base v1.Image) (refs map[string]string, stop func(), err error) {
	layer, err := testLayer()
	if err != nil {
		return nil, nil, err
	}

	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		return nil, nil, fmt.Errorf("add test layer: %w", err)
	}

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	host := strings.TrimPrefix(srv.URL, "http://")

	const path = "PATH=/bin"

	configs := map[string]v1.Config{
		Busybox: {Cmd: []string{"sh"}, Env: []string{path}},
		Configured: {
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{"echo $A $B; id -u; id -g; pwd"},
			Env:        []string{path, "A=image", "B=image"},
			WorkingDir: "/tmp",
			User:       "1000:1000",
		},
		NamedUser: {Cmd: []string{"sh"}, Env: []string{path}, User: "nobody"},
	}

	refs = map[string]string{MissingImage: host + "/" + MissingImage}

	for name, cfg := range configs {
		refs[name] = host + "/" + name

		configured, err := mutate.Config(img, cfg)
		if err == nil {
			err = push(ctx, refs[name], configured)
		}

		if err != nil {
			srv.Close()

			return nil, nil, fmt.Errorf("serve image %s: %w", name, err)
		}
	}

	return refs, srv.Close, nil
}

func push(ctx context.Context, ref string, img v1.Image) error {
	r, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}

	err = remote.Write(r, img, remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}

	return nil
}

// testLayer adds files whose owner, mode, and hardlink the guest must see,
// and an image-provided /.krun_config.json, a symlink to HijackFile, that must not change what runs.
func testLayer() (v1.Layer, error) {
	hijack := `{"uid":"0","gid":"0","WorkingDir":"/","Env":["PATH=/bin"],"Entrypoint":[],"args":["echo","hijacked"]}`
	headers := []*tar.Header{
		{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: strings.TrimPrefix(SetuidFile, "/"), Typeflag: tar.TypeReg, Mode: 0o4755, Gid: 42, Size: 1},
		// Like GNU tar, the hardlink header repeats the owner and mode of its target.
		// containerd applies them to the shared inode, so headers without them reset the target.
		{Name: strings.TrimPrefix(LinkFile, "/"), Typeflag: tar.TypeLink, Linkname: strings.TrimPrefix(SetuidFile, "/"), Mode: 0o4755, Gid: 42},
		{Name: "data/hijack.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(hijack))},
		{Name: ".krun_config.json", Typeflag: tar.TypeSymlink, Linkname: "/data/hijack.json", Mode: 0o777},
	}
	bodies := map[string]string{headers[1].Name: "x", headers[3].Name: hijack}

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, hdr := range headers {
		hdr.ModTime = time.Unix(0, 0)

		err := tw.WriteHeader(hdr)
		if err == nil {
			_, err = io.WriteString(tw, bodies[hdr.Name])
		}

		if err != nil {
			return nil, fmt.Errorf("write test layer: %w", err)
		}
	}

	err := tw.Close()
	if err != nil {
		return nil, fmt.Errorf("write test layer: %w", err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		return nil, fmt.Errorf("create test layer: %w", err)
	}

	return layer, nil
}
