package main

import (
	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/userns"
)

// isolated runs image work and VMs inside a user namespace, where image files get real owners
// and each run root is an overlay. The process outside runs the checks and then the same command inside.
func isolated(b cli.Backend) cli.Backend {
	if !userns.Inside() {
		b.Isolate = userns.Run

		return b
	}

	b.Checks = func(string) []preflight.Check { return nil }
	b.Images = func(cacheDir string) (image.Store, error) {
		c, err := image.Open(cacheDir, image.OwnershipChown)

		return image.Store{Cache: c, Overlay: true, Progress: nil}, err //nolint:wrapcheck // Open errors name the cache.
	}

	return b
}
