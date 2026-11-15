package run

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"testing"

	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/krunconfig"
	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

func TestWithVolumes(t *testing.T) {
	t.Parallel()

	cfg := krunconfig.Config{UID: 1000, GID: 42, WorkingDir: "/a/b", Env: []string{"PATH=/bin"}, Args: []string{"sh", "-c", "pwd"}}
	volumes := []spec.Volume{
		{Host: "/h/ab", Guest: "/a/b", ReadOnly: false},
		{Host: "/h/a-b", Guest: "/a-b", ReadOnly: true},
		{Host: "/h/a", Guest: "/a", ReadOnly: false},
	}

	got, devices := withVolumes(volumes, cfg)

	tag := func(guest string) string {
		sum := sha256.Sum256([]byte(guest))

		return "sbxr-" + hex.EncodeToString(sum[:8])
	}

	// Sorted by guest path, a parent mounts before the volume inside it.
	wantDevices := []vm.Volume{
		{Tag: tag("/a"), Host: "/h/a", ReadOnly: false},
		{Tag: tag("/a-b"), Host: "/h/a-b", ReadOnly: true},
		{Tag: tag("/a/b"), Host: "/h/ab", ReadOnly: false},
	}
	if !reflect.DeepEqual(devices, wantDevices) {
		t.Errorf("devices = %+v, want %+v", devices, wantDevices)
	}

	for _, d := range devices {
		if !regexp.MustCompile(`^sbxr-[0-9a-f]{16}$`).MatchString(d.Tag) {
			t.Errorf("tag %q, want sbxr- and 16 hex digits", d.Tag)
		}
	}

	wantArgs := []string{
		guestmount.Path, "1000:42", "/a/b", "3",
		tag("/a") + ":rw:/a", tag("/a-b") + ":ro:/a-b", tag("/a/b") + ":rw:/a/b",
		"sh", "-c", "pwd",
	}
	if got.UID != 0 || got.GID != 0 || got.WorkingDir != cfg.WorkingDir || !reflect.DeepEqual(got.Env, cfg.Env) || !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("config = %+v, want uid 0, gid 0, the same workdir and env, and args %q", got, wantArgs)
	}

	unchanged, none := withVolumes(nil, cfg)
	if !reflect.DeepEqual(unchanged, cfg) || none != nil {
		t.Errorf("withVolumes without volumes = %+v, %v, want the config unchanged", unchanged, none)
	}
}
