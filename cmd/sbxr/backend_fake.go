//go:build sbxrfake

package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/vm"
	"github.com/albertsko/sbxr/internal/vm/fake"
)

// This build runs workloads with the fake hypervisor, images from SBXR_FAKE_IMAGES,
// and preflight checks that fail only when listed in SBXR_FAKE_PREFLIGHT_FAIL.
func hypervisor() vm.Hypervisor { return fake.Hypervisor{} }

func backend() cli.Backend {
	return cli.Backend{
		Checks: func(string) []preflight.Check { return fake.Checks(os.Getenv("SBXR_FAKE_PREFLIGHT_FAIL")) },
		Images: func(string) run.Images { return fake.Images{Dir: os.Getenv("SBXR_FAKE_IMAGES")} },
	}
}
