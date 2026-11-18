// Package testenv decides what a vm or kube test does when its environment is missing.
package testenv

import (
	"os"
	"testing"
)

// RequireVar is the environment variable that turns skips for a missing environment into failures.
const RequireVar = "SBXR_TEST_REQUIRE"

// SkipOrFailf ends tb because its environment is missing.
// It skips tb, or fails tb when SBXR_TEST_REQUIRE=1, so that CI never skips tests silently.
func SkipOrFailf(tb testing.TB, format string, args ...any) {
	tb.Helper()

	if os.Getenv(RequireVar) == "1" {
		tb.Fatalf(format+" ("+RequireVar+"=1 requires it)", args...)

		return
	}

	tb.Skipf(format, args...)
}
