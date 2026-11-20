package run

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/runspec"
)

func TestWorkload(t *testing.T) {
	t.Parallel()

	full := image.Config{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		Env:        []string{"PATH=/bin", "A=image", "KRUN_INIT=/x", "HOSTNAME=img", "B=\x1b"},
		WorkingDir: "/app",
		User:       "100:200",
	}
	empty := ""

	tests := []struct {
		name         string
		spec         runspec.Spec
		image        image.Config
		want         krunconfig.Config
		wantWarnings int
	}{
		{
			name:         "image values apply",
			image:        full,
			want:         krunconfig.Config{UID: 100, GID: 200, WorkingDir: "/app", Env: []string{"PATH=/bin", "A=image", "HOME=/", "TERM=linux"}, Args: []string{"/entry", "default"}},
			wantWarnings: 3,
		},
		{
			name:  "flags override the image",
			spec:  runspec.Spec{Args: []string{"run", "uid 0"}, Env: []string{"A=one", "C=c", "A=two", "TERM=xterm"}, Workdir: "/w", User: &runspec.User{UID: 1000}},
			image: image.Config{Entrypoint: []string{"/entry"}, Env: []string{"A=image"}, User: "5"},
			want:  krunconfig.Config{UID: 1000, GID: 0, WorkingDir: "/w", Env: []string{"A=two", "C=c", "TERM=xterm", "HOME=/", "PATH=" + DefaultPath}, Args: []string{"/entry", "run", "uid 0"}},
		},
		{
			name:  "entrypoint flag drops the image command",
			spec:  runspec.Spec{Entrypoint: new("/bin/echo")},
			image: image.Config{Entrypoint: []string{"/entry"}, Cmd: []string{"default"}},
			want:  krunconfig.Config{WorkingDir: "/", Env: []string{"HOME=/", "TERM=linux", "PATH=" + DefaultPath}, Args: []string{"/bin/echo"}},
		},
		{
			name:  "empty entrypoint flag clears the entrypoint",
			spec:  runspec.Spec{Entrypoint: &empty, Args: []string{"sh"}},
			image: image.Config{Entrypoint: []string{"/entry"}},
			want:  krunconfig.Config{WorkingDir: "/", Env: []string{"HOME=/", "TERM=linux", "PATH=" + DefaultPath}, Args: []string{"sh"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The image root has no /etc/passwd, so only numeric users resolve.
			got, warnings, err := Workload(tt.spec, &image.Image{Root: t.TempDir(), Config: tt.image})
			if err != nil {
				t.Fatalf("Workload() error = %v", err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Workload() =\n%+v\nwant\n%+v", got, tt.want)
			}

			if len(warnings) != tt.wantWarnings {
				t.Errorf("Workload() warnings = %q, want %d", warnings, tt.wantWarnings)
			}

			_, err = krunconfig.Marshal(got)
			if err != nil {
				t.Errorf("Marshal(Workload()) error = %v", err)
			}
		})
	}
}

func TestWorkloadResolvesImageUserOnlyWithoutUserFlag(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	err := os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("root:x:0:0::/root:/bin/sh\napp:x:7:8::/app:/bin/sh\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		spec    runspec.Spec
		user    string
		wantUID uint32
		wantGID uint32
		wantErr string
	}{
		{name: "named user", user: "app", wantUID: 7, wantGID: 8},
		{name: "missing user", user: "nobody", wantErr: `image "img": user "nobody": no such user in the image /etc/passwd`},
		{name: "user flag wins over a missing user", spec: runspec.Spec{User: &runspec.User{UID: 1, GID: 2}}, user: "nobody", wantUID: 1, wantGID: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.spec.Image = "img"
			img := &image.Image{Root: root, Config: image.Config{User: tt.user, Cmd: []string{"sh"}}}

			got, _, err := Workload(tt.spec, img)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Errorf("Workload() error = %v, want %q", err, tt.wantErr)
				}

				return
			}

			if err != nil || got.UID != tt.wantUID || got.GID != tt.wantGID {
				t.Errorf("Workload() = %+v, %v; want uid %d and gid %d", got, err, tt.wantUID, tt.wantGID)
			}
		})
	}
}
