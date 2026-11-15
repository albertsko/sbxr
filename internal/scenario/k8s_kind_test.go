//go:build kind

package scenario_test

import (
	"testing"

	"github.com/albertsko/sbxr/internal/scenario"
)

// TestKindTarget runs the scenarios that match the Kubernetes phase 1 capabilities against a kind cluster.
func TestKindTarget(t *testing.T) {
	t.Parallel()

	scenario.RequireHypervisor(t)
	t.Skip("needs run: S7")
}
