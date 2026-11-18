package preflight

import (
	"context"
	"fmt"
	"os"

	"github.com/albertsko/sbxr/internal/hv"
)

const (
	hypervisorDetail = "Hypervisor.framework available"
	libkrunInstall   = "brew install libkrun"
	krunfwInstall    = "brew upgrade libkrunfw"
)

var platformChecks = []Check{Entitlement}

func hypervisorFix(error) string {
	return "use a Mac with Apple silicon and macOS 15 or later"
}

// Entitlement checks that the running binary is signed with the hypervisor entitlement.
func Entitlement(context.Context) Result {
	const id = "entitlement"

	exe, err := os.Executable()
	if err != nil {
		return Failed(id, fmt.Sprintf("find the sbxr binary: %v", err), "run sbxr from its installed path")
	}

	err = hv.CheckEntitlement(exe)
	if err != nil {
		return Failed(id, err.Error(), fmt.Sprintf(
			"codesign --force --sign - --entitlements entitlements.plist %q (entitlements.plist is in the release archive and at cmd/sbxr/entitlements.plist in the sbxr repository)", exe))
	}

	return Passed(id, exe+" has the "+hv.Entitlement+" entitlement")
}
