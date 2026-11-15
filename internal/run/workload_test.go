package run

import (
	"cmp"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/spec"
)

func TestWorkload(t *testing.T) {
	t.Parallel()

	image := ImageConfig{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		Env:        []string{"PATH=/bin", "A=image", "KRUN_INIT=/x", "HOSTNAME=img", "B=\x1b"},
		WorkingDir: "/app",
		User:       "100:200",
	}
	empty := ""

	tests := []struct {
		name         string
		spec         spec.Spec
		image        ImageConfig
		want         krunconfig.Config
		wantWarnings int
	}{
		{
			name:         "image values apply",
			image:        image,
			want:         krunconfig.Config{UID: 100, GID: 200, WorkingDir: "/app", Env: []string{"PATH=/bin", "A=image", "HOME=/", "TERM=linux"}, Args: []string{"/entry", "default"}},
			wantWarnings: 3,
		},
		{
			name:  "flags override the image",
			spec:  spec.Spec{Args: []string{"run", "uid 0"}, Env: []string{"A=one", "C=c", "A=two", "TERM=xterm"}, Workdir: "/w", User: &spec.User{UID: 1000}},
			image: ImageConfig{Entrypoint: []string{"/entry"}, Env: []string{"A=image"}, User: "5"},
			want:  krunconfig.Config{UID: 1000, GID: 0, WorkingDir: "/w", Env: []string{"A=two", "C=c", "TERM=xterm", "HOME=/", "PATH=" + DefaultPath}, Args: []string{"/entry", "run", "uid 0"}},
		},
		{
			name:  "entrypoint flag drops the image command",
			spec:  spec.Spec{Entrypoint: new("/bin/echo")},
			image: ImageConfig{Entrypoint: []string{"/entry"}, Cmd: []string{"default"}},
			want:  krunconfig.Config{WorkingDir: "/", Env: []string{"HOME=/", "TERM=linux", "PATH=" + DefaultPath}, Args: []string{"/bin/echo"}},
		},
		{
			name:  "empty entrypoint flag clears the entrypoint",
			spec:  spec.Spec{Entrypoint: &empty, Args: []string{"sh"}},
			image: ImageConfig{Entrypoint: []string{"/entry"}},
			want:  krunconfig.Config{WorkingDir: "/", Env: []string{"HOME=/", "TERM=linux", "PATH=" + DefaultPath}, Args: []string{"sh"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, warnings, err := Workload(tt.spec, tt.image, numericUser)
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

	resolver := func(user string) (uint32, uint32, error) {
		if user != "app" {
			return 0, 0, errors.New("unknown user")
		}

		return 7, 8, nil
	}

	got, _, err := Workload(spec.Spec{}, ImageConfig{User: "app", Cmd: []string{"sh"}}, resolver)
	if err != nil || got.UID != 7 || got.GID != 8 {
		t.Errorf("Workload() = %+v, %v; want uid 7 and gid 8", got, err)
	}

	_, _, err = Workload(spec.Spec{}, ImageConfig{User: "nobody", Cmd: []string{"sh"}}, resolver)
	if err == nil {
		t.Error("Workload() with an unknown image user succeeded, want an error")
	}

	got, _, err = Workload(spec.Spec{User: &spec.User{UID: 1}}, ImageConfig{User: "nobody", Cmd: []string{"sh"}}, resolver)
	if err != nil || got.UID != 1 {
		t.Errorf("Workload() with -u = %+v, %v; want uid 1 without resolving the image user", got, err)
	}
}

// numericUser resolves uid[:gid] values like an image without /etc/passwd.
func numericUser(user string) (uint32, uint32, error) {
	if user == "" {
		return 0, 0, nil
	}

	uidRaw, gidRaw, _ := strings.Cut(user, ":")

	uid, err1 := strconv.ParseUint(uidRaw, 10, 32)
	gid, err2 := strconv.ParseUint(cmp.Or(gidRaw, "0"), 10, 32)

	return uint32(uid), uint32(gid), errors.Join(err1, err2)
}
