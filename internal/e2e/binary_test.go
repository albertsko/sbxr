package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// buildDir holds the binaries that the tests build. TestMain removes it.
var buildDir string

var (
	releaseOnce sync.Once
	releaseBin  string
	errRelease  error
)

// releaseBinary returns the release build of cmd/sbxr, built once per test process with cgo off.
// On macOS, it is signed with the hypervisor entitlement, so that it can start VMs.
func releaseBinary(t *testing.T) string {
	t.Helper()

	releaseOnce.Do(func() {
		releaseBin = filepath.Join(buildDir, "sbxr")
		errRelease = build(context.Background(), releaseBin)
	})

	if errRelease != nil {
		t.Fatal(errRelease)
	}

	return releaseBin
}

// build generates the guest mount helper, which the release embeds, and builds cmd/sbxr into out.
func build(ctx context.Context, out string) error {
	gen := exec.CommandContext(ctx, "go", "generate", "github.com/albertsko/sbxr/internal/guestmount")
	if output, err := gen.CombinedOutput(); err != nil {
		return fmt.Errorf("generate the mount helper: %w\n%s", err, output)
	}

	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "github.com/albertsko/sbxr/cmd/sbxr")

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build sbxr: %w\n%s", err, output)
	}

	if runtime.GOOS != "darwin" {
		return nil
	}

	output, err = exec.CommandContext(ctx, "codesign", "--force", "--sign", "-", "--entitlements", "../../cmd/sbxr/entitlements.plist", out).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign: %w\n%s", err, output)
	}

	return nil
}
