//go:build !sbxrfake

package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/guestmount"
	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/vm"
	"github.com/albertsko/sbxr/internal/vm/libkrun"
)

func hypervisor() vm.Hypervisor { return libkrun.Hypervisor{LibDir: os.Getenv("SBXR_LIB_DIR")} }

func backend() cli.Backend {
	b := cli.Backend{
		Checks: func(cacheDir string) []preflight.Check {
			return preflight.Checks(cacheDir, os.Getenv("SBXR_LIB_DIR"))
		},
		Images: func(cacheDir string) (image.Store, error) {
			c, err := image.Open(cacheDir, image.OwnershipXattr)

			return image.Store{Cache: c, Overlay: false, Progress: nil}, err //nolint:wrapcheck // Open errors name the cache.
		},
		Isolate:     nil,
		VMEnv:       nil,
		MountHelper: guestmount.Binary,
	}

	// Without a library directory the libkrun check fails first, so an empty environment is fine.
	if dir, err := libkrun.LibDir(os.Getenv("SBXR_LIB_DIR")); err == nil {
		b.VMEnv = libkrun.Env(dir)
	}

	return isolated(b)
}
