package hv

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPlistTrue(t *testing.T) {
	t.Parallel()

	const header = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
`

	tests := []struct {
		name  string
		plist string
		want  bool
	}{
		{"true", `<plist version="1.0"><dict><key>com.apple.security.hypervisor</key><true/></dict></plist>`, true},
		{"true with whitespace", "<plist>\n<dict>\n  <key>com.apple.security.hypervisor</key>\n  <true/>\n</dict>\n</plist>", true},
		{"false", `<plist><dict><key>com.apple.security.hypervisor</key><false/></dict></plist>`, false},
		{"missing", `<plist><dict><key>com.apple.security.app-sandbox</key><true/></dict></plist>`, false},
		{"value is the key name", `<plist><dict><key>other</key><string>com.apple.security.hypervisor</string><true/></dict></plist>`, false},
		{"empty", ``, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := plistTrue([]byte(header+tt.plist), Entitlement)
			if err != nil {
				t.Fatalf("plistTrue() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("plistTrue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckEntitlementRejectsNonMachO(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckEntitlement(path); err == nil {
		t.Error("CheckEntitlement() = nil, want error")
	}
}

func TestCheckEntitlementSignedBinary(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("codesign exists only on macOS")
	}

	tests := []struct {
		name    string
		sign    []string
		wantErr bool
	}{
		{"unsigned", []string{"--remove-signature"}, true},
		{"no entitlements", []string{"--force", "--sign", "-"}, true},
		{"hypervisor entitlement", []string{"--force", "--sign", "-", "--entitlements", "../../cmd/sbxr/entitlements.plist"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := copyTestBinary(t)

			out, err := exec.CommandContext(t.Context(), "codesign", append(tt.sign, path)...).CombinedOutput()
			if err != nil {
				t.Fatalf("codesign: %v: %s", err, out)
			}

			err = CheckEntitlement(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckEntitlement() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// copyTestBinary returns a copy of the running test binary, which is a Mach-O file on macOS.
func copyTestBinary(t *testing.T) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	path := filepath.Join(t.TempDir(), "bin")

	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}

	return path
}
