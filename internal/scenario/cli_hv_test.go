//go:build hv

package scenario_test

import (
	"testing"

	"github.com/albertsko/sbxr/internal/scenario"
)

// TestHVCLITarget runs the scenarios against the release build of sbxr with a real VM.
func TestHVCLITarget(t *testing.T) {
	t.Parallel()

	scenario.RequireHypervisor(t)
	t.Skip("needs run: S3")
}
