package sbxrmain

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	t.Parallel()

	const devVersion = "dev"

	withVersion := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{
			GoVersion: "",
			Path:      "",
			Main: debug.Module{
				Path:    "",
				Version: v,
				Sum:     "",
				Replace: nil,
			},
			Deps:     nil,
			Settings: nil,
		}
	}

	tests := []struct {
		name   string
		linked string
		info   *debug.BuildInfo
		want   string
	}{
		{name: "goreleaser version wins", linked: "1.2.3", info: withVersion("v9.9.9"), want: "1.2.3"},
		{name: "go install version", linked: devVersion, info: withVersion("v1.4.0"), want: "v1.4.0"},
		{name: "local build", linked: devVersion, info: withVersion("(devel)"), want: devVersion},
		{name: "empty module version", linked: devVersion, info: withVersion(""), want: devVersion},
		{name: "no build info", linked: devVersion, info: nil, want: devVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resolveVersion(tt.linked, tt.info); got != tt.want {
				t.Errorf("resolveVersion(%q, %v) = %q, want %q", tt.linked, tt.info, got, tt.want)
			}
		})
	}
}
