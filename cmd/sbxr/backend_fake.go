//go:build sbxrfake

package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/vm"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

// This build runs workloads with the fake hypervisor, keeps image ownership in xattrs on every platform,
// and runs preflight checks that fail only when listed in SBXR_FAKE_PREFLIGHT_FAIL.
func hypervisor() vm.Hypervisor { return fake.Hypervisor{} }

func backend() cli.Backend {
	return cli.Backend{
		Checks: func(string) []preflight.Check { return fake.Checks(os.Getenv("SBXR_FAKE_PREFLIGHT_FAIL")) },
		Images: func(cacheDir string) (image.Store, error) {
			c, err := image.Open(cacheDir, image.OwnershipXattr)

			return image.Store{Cache: c, Overlay: false}, err //nolint:wrapcheck // Open errors name the cache.
		},
		Isolate: nil,
		VMEnv:   nil,
	}
}
