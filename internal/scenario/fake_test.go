package scenario_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/albertsko/sbxr/internal/scenario"
)

var fakeBin string

func TestMain(m *testing.M) {
	os.Exit(buildAndRun(m))
}

func buildAndRun(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sbxr-scenario-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer os.RemoveAll(dir)

	fakeBin = filepath.Join(dir, "sbxr")

	err = scenario.Build(context.Background(), fakeBin, "sbxrfake")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	return m.Run()
}

func TestFakeTarget(t *testing.T) {
	t.Parallel()

	refs := scenario.FakeImages(t)

	scenario.RunAll(t, func(t *testing.T) scenario.Target {
		t.Helper()

		return scenario.Fake(t, fakeBin, refs)
	})
}
