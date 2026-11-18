// Command sbxr runs OCI images as small virtual machines.
package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/sbxrmain"
)

// version is set by goreleaser through -ldflags.
var version = "dev"

func init() { sbxrmain.LockMainThread() }

func main() { os.Exit(sbxrmain.Main(version, hypervisor, backend)) }
