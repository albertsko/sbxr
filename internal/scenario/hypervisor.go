package scenario

import (
	"testing"

	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/testenv"
)

// RequireHypervisor skips t when the `hypervisor` doctor check fails, or fails t when SBXR_TEST_REQUIRE=1.
func RequireHypervisor(t *testing.T) {
	t.Helper()

	res := preflight.Hypervisor(t.Context())
	if res.Status == preflight.Pass {
		return
	}

	testenv.SkipOrFailf(t, "no hypervisor: %s", res.Detail)
}
