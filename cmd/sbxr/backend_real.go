//go:build !sbxrfake

package main

import (
	"context"
	"errors"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm"
)

// errNotBuilt marks the parts that later build steps add: the libkrun binding and image pull.
var errNotBuilt = errors.New("not implemented yet")

func hypervisor() vm.Hypervisor { return notBuilt{} }

func backend() cli.Backend {
	return cli.Backend{
		Checks: func(cacheDir string) []preflight.Check {
			return []preflight.Check{preflight.Hypervisor, preflight.Cache(cacheDir)}
		},
		Images: func(string) run.Images { return notBuilt{} },
	}
}

type notBuilt struct{}

func (notBuilt) Start(vm.Machine) error { return errNotBuilt }

func (notBuilt) Pull(context.Context, string, spec.Pull) (run.Image, error) {
	return run.Image{}, errNotBuilt
}

func (notBuilt) NewRoot(context.Context, run.Image, string) (string, error) { return "", errNotBuilt }
