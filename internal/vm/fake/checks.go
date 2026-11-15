package fake

import (
	"context"
	"runtime"
	"slices"
	"strings"

	"github.com/albertsko/sbxr/internal/preflight"
)

// Checks returns one check per doctor check ID of this platform.
// Each check passes, except the IDs in failIDs, a comma-separated list.
func Checks(failIDs string) []preflight.Check {
	ids := []string{"hypervisor", "libkrun", "libkrunfw"}

	switch runtime.GOOS {
	case "darwin":
		ids = append(ids, "entitlement")
	case "linux":
		ids = append(ids, "userns")
	}

	ids = append(ids, "cache")
	failing := strings.Split(failIDs, ",")

	checks := make([]preflight.Check, 0, len(ids))
	for _, id := range ids {
		checks = append(checks, func(context.Context) preflight.Result {
			if slices.Contains(failing, id) {
				return preflight.Failed(id, "fake "+id+" failure", "fake fix for "+id)
			}

			return preflight.Passed(id, "fake "+id+" check")
		})
	}

	return checks
}
