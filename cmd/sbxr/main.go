// Command sbxr runs OCI images as small virtual machines.
package main

import (
	"os"
	"runtime"
	"runtime/debug"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/vm"
)

// version is set by goreleaser through -ldflags.
var version = "dev"

// init keeps the VM process on the main thread, where the hypervisor must run.
// It runs in init, so that main runs on the locked thread.
func init() {
	if len(os.Args) > 1 && os.Args[1] == vm.ChildArg {
		runtime.LockOSThread()
	}
}

func main() { os.Exit(run()) }

func run() int {
	if code, done := platformMain(os.Args[1:]); done {
		return code
	}

	// The VM process is a hidden mode of the same binary, dispatched before the CLI.
	if len(os.Args) > 1 && os.Args[1] == vm.ChildArg {
		return vm.Child(os.Args[2:], hypervisor(), os.Stderr)
	}

	info, _ := debug.ReadBuildInfo()

	return cli.Main(os.Args[1:], resolveVersion(version, info), backend())
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
