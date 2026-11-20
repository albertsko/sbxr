//go:build vm

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/preflight"
)

func TestVMDoctorPasses(t *testing.T) {
	t.Parallel()

	res := sbxr(t, vmBinary(t), vmEnv(vmCacheDir(t)), "doctor", "--json")

	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}

	err := json.Unmarshal([]byte(res.stdout), &report)
	if err != nil || res.code != 0 || !report.OK {
		t.Fatalf("sbxr doctor --json = %d, %q, %v; want 0 and ok", res.code, res.stdout, err)
	}
}

// TestVMDoctorPassesWithoutSetupBlock checks that a passing doctor report has no Linux setup block.
// TestDoctorSetupBlock checks a failed report.
func TestVMDoctorPassesWithoutSetupBlock(t *testing.T) {
	t.Parallel()

	res := sbxr(t, vmBinary(t), vmEnv(vmCacheDir(t)), "doctor")
	if res.code != 0 || strings.Contains(res.stdout, preflight.LinuxSetup) {
		t.Errorf("sbxr doctor = %d, %q; want 0 and no setup block", res.code, res.stdout)
	}
}

func TestVMFailedPreflightStartsNoVM(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	refs := vmImages(t)
	cacheDir := vmCacheDir(t)
	noLib := filepath.Join(t.TempDir(), "nolib")
	environ := append(vmEnv(cacheDir), "SBXR_LIB_DIR="+noLib)

	// The library file names depend on the host, so the check skips the middle of the line.
	res := sbxr(t, bin, environ, "run", refs[Busybox], "echo", "hi")

	wantStart, wantEnd := `sbxr: preflight "libkrun": `, " not found in "+noLib+"; set SBXR_LIB_DIR to the directory that holds libkrun and libkrunfw\n"
	if res.code != 125 || res.stdout != "" || !strings.HasPrefix(res.stderr, wantStart) || !strings.HasSuffix(res.stderr, wantEnd) || strings.Count(res.stderr, "\n") != 1 {
		t.Errorf("sbxr run = %d, stdout %q, stderr %q; want 125 and one line %q...%q", res.code, res.stdout, res.stderr, wantStart, wantEnd)
	}

	err := CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}

	res = sbxr(t, bin, environ, "doctor")

	_, fixAll, _ := strings.Cut(res.stdout, "Fix all:")
	if res.code != 1 || !strings.Contains(res.stdout, "fail  libkrun") || !strings.Contains(fixAll, "SBXR_LIB_DIR") {
		t.Errorf("sbxr doctor = %d, %q; want 1, a libkrun failure, and a combined fix", res.code, res.stdout)
	}
}

// TestVMAppArmorNeedsInstallPath checks the userns fix for a binary outside the profile paths.
func TestVMAppArmorNeedsInstallPath(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Skip("AppArmor does not restrict user namespaces here")
	}

	cacheDir := vmCacheDir(t)

	// The release build lives in a temporary directory, outside the profile paths.
	res := sbxr(t, releaseBinary(t), vmEnv(cacheDir), "doctor")

	// The setup block at the end installs to /usr/local/bin, so only the report above it counts.
	out, _ := strings.CutSuffix(res.stdout, preflight.LinuxSetup)
	if res.code != 1 || !strings.Contains(out, "fail  userns") || !strings.Contains(out, "install sbxr to /usr/local/bin") || strings.Contains(out, "apparmor_parser") {
		t.Errorf("sbxr doctor = %d, %q; want a userns failure that asks for /usr/local/bin and prints no profile command", res.code, res.stdout)
	}
}
