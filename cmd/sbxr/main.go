// Command sbxr runs OCI images as small virtual machines.
package main

import (
	"os"
	"runtime/debug"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/vm"
)

// version is set by goreleaser through -ldflags.
var version = "dev"

func main() {
	// The VM process is a hidden mode of the same binary, dispatched before the CLI.
	if len(os.Args) > 1 && os.Args[1] == vm.ChildArg {
		os.Exit(vm.Child(os.Args[2:], hypervisor(), os.Stderr))
	}

	info, _ := debug.ReadBuildInfo()
	os.Exit(cli.Main(os.Args[1:], resolveVersion(version, info), backend()))
}

// resolveVersion falls back to the module version that go install records.
func resolveVersion(linked string, info *debug.BuildInfo) string {
	if linked != "dev" || info == nil {
		return linked
	}

	if info.Main.Version == "" || info.Main.Version == "(devel)" {
		return linked
	}

	return info.Main.Version
}
