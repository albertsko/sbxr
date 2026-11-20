// Package sbxrmain is the startup code of cmd/sbxr.
package sbxrmain

import (
	"os"
	"runtime"
	"runtime/debug"

	"github.com/albertsko/sbxr/internal/cli"
	"github.com/albertsko/sbxr/internal/vm"
)

// LockMainThread keeps the VM process on the main thread, where the hypervisor must run.
// Call it from an init function of a main package, so that main runs on the locked thread.
func LockMainThread() {
	if len(os.Args) > 1 && os.Args[1] == vm.ChildArg {
		runtime.LockOSThread()
	}
}

// Main runs sbxr with the given hypervisor and backend and returns the exit code.
func Main(version string, hypervisor func() vm.Hypervisor, backend func() cli.Backend) int {
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
