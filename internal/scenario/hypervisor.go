package scenario

import (
	"os"
	"testing"

	"github.com/albertsko/sbxr/internal/preflight"
)

// RequireHypervisor skips t when the `hypervisor` doctor check fails.
// With SBXR_REQUIRE_HV=1, it fails t instead, so that CI never skips VM tests silently.
func RequireHypervisor(t *testing.T) {
	t.Helper()

	res := preflight.Hypervisor(t.Context())
	if res.Status == preflight.Pass {
		return
	}

	if os.Getenv("SBXR_REQUIRE_HV") == "1" {
		t.Fatalf("no hypervisor, and SBXR_REQUIRE_HV=1 requires one: %s", res.Detail)
	}

	t.Skipf("no hypervisor: %s", res.Detail)
}
