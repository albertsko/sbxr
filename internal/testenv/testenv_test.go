package testenv_test

import (
	"fmt"
	"testing"

	"github.com/albertsko/sbxr/internal/testenv"
)

// recorder records how SkipOrFailf ends a test instead of ending it.
type recorder struct {
	testing.TB

	skipped, failed string
}

func (r *recorder) Helper() {}

func (r *recorder) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

func (r *recorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func TestSkipOrFailf(t *testing.T) {
	tests := []struct {
		name        string
		value       string
		wantSkipped string
		wantFailed  string
	}{
		{name: "unset skips", value: "", wantSkipped: "no hypervisor: x"},
		{name: "other value skips", value: "true", wantSkipped: "no hypervisor: x"},
		{name: "1 fails", value: "1", wantFailed: "no hypervisor: x (SBXR_TEST_REQUIRE=1 requires it)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(testenv.RequireVar, tt.value)

			r := &recorder{TB: t}
			testenv.SkipOrFailf(r, "no hypervisor: %s", "x")

			if r.skipped != tt.wantSkipped || r.failed != tt.wantFailed {
				t.Errorf("SkipOrFailf skipped %q and failed %q, want %q and %q", r.skipped, r.failed, tt.wantSkipped, tt.wantFailed)
			}
		})
	}
}
