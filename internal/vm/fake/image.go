package fake

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"runtime"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Image returns a busybox-like image for the host architecture.
// Its root has the fake busybox in /bin with one symlink per applet, /etc/passwd, /etc/group, /root, and /tmp.
func Image() (v1.Image, error) {
	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	mtime := time.Unix(0, 0)

	headers := make([]*tar.Header, 0, 7+len(Applets))
	headers = append(headers,
		&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "root/", Typeflag: tar.TypeDir, Mode: 0o700},
		&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777},
	)

	const busybox = "bin/busybox"

	files := map[string]string{ //nolint:gosec // These are image files, not credentials.
		busybox:      Marker,
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/home:/bin/false\n",
		"etc/group":  "root:x:0:\nnobody:x:65534:\n",
	}

	for _, name := range []string{busybox, "etc/passwd", "etc/group"} {
		mode := int64(0o644)
		if name == busybox {
			mode = 0o755
		}

		headers = append(headers, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(files[name]))})
	}

	for _, applet := range Applets {
		headers = append(headers, &tar.Header{Name: "bin/" + applet, Typeflag: tar.TypeSymlink, Linkname: "busybox", Mode: 0o777})
	}

	for _, hdr := range headers {
		hdr.ModTime = mtime

		err := tw.WriteHeader(hdr)
		if err == nil {
			_, err = io.WriteString(tw, files[hdr.Name])
		}

		if err != nil {
			return nil, fmt.Errorf("write fake image layer: %w", err)
		}
	}

	err := tw.Close()
	if err != nil {
		return nil, fmt.Errorf("write fake image layer: %w", err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		return nil, fmt.Errorf("create fake image layer: %w", err)
	}

	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: runtime.GOARCH})
	if err != nil {
		return nil, fmt.Errorf("create fake image: %w", err)
	}

	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		return nil, fmt.Errorf("create fake image: %w", err)
	}

	return img, nil
}
