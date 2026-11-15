package preflight_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/preflight"
)

func TestRunReportsEveryCheck(t *testing.T) {
	t.Parallel()

	pass := func(context.Context) preflight.Result { return preflight.Passed("hypervisor", "available") }
	fail := func(context.Context) preflight.Result {
		return preflight.Failed("libkrun", "libkrun.1.dylib not found", "brew install libkrun")
	}

	report := preflight.Run(t.Context(), []preflight.Check{pass, fail})

	got, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"schemaVersion":1,"ok":false,"checks":[` +
		`{"id":"hypervisor","status":"pass","detail":"available"},` +
		`{"id":"libkrun","status":"fail","detail":"libkrun.1.dylib not found","fix":"brew install libkrun"}]}`
	if string(got) != want {
		t.Errorf("report JSON =\n%s\nwant\n%s", got, want)
	}

	if failed := report.Failures(); len(failed) != 1 || failed[0].ID != "libkrun" {
		t.Errorf("Failures() = %+v, want only libkrun", failed)
	}
}

func TestCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readOnly := filepath.Join(dir, "ro")

	err := os.Mkdir(readOnly, 0o500)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
		want preflight.Status
	}{
		{name: "new directory", dir: filepath.Join(dir, "a", "b"), want: preflight.Pass},
		{name: "read-only directory", dir: filepath.Join(readOnly, "cache"), want: preflight.Fail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := preflight.Cache(tt.dir)(t.Context())
			if res.ID != "cache" || res.Status != tt.want {
				t.Errorf("Cache(%q) = %+v, want status %s", tt.dir, res, tt.want)
			}

			if (res.Fix != "") != (tt.want == preflight.Fail) {
				t.Errorf("Cache(%q) fix = %q, want a fix only on failure", tt.dir, res.Fix)
			}
		})
	}
}

func TestREADMEHasLinuxSetup(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(readme), "```sh\n"+preflight.LinuxSetup+"```\n") {
		t.Errorf("README.md has no sh block equal to preflight.LinuxSetup:\n%s", preflight.LinuxSetup)
	}
}

// TestLinuxSetupSubIDs runs the subordinate id line of the setup block against temporary files.
func TestLinuxSetupSubIDs(t *testing.T) {
	t.Parallel()

	var line string

	for l := range strings.Lines(preflight.LinuxSetup) {
		if strings.Contains(l, "/etc/subuid") {
			line = l
		}
	}

	tests := []struct {
		name                   string
		subuid, subgid         string
		wantSubuid, wantSubgid string
	}{
		{
			name:       "empty files",
			wantSubuid: "alice:100000:65536\n",
			wantSubgid: "alice:100000:65536\n",
		},
		{
			name:       "after the highest range",
			subuid:     "bob:100000:65536\ncarol:300000:1000\ndan:200000:65536\n",
			subgid:     "bob:1000:10\n",
			wantSubuid: "bob:100000:65536\ncarol:300000:1000\ndan:200000:65536\nalice:301000:65536\n",
			wantSubgid: "bob:1000:10\nalice:100000:65536\n",
		},
		{
			name:       "only the missing file",
			subuid:     "alice:100000:65536\n",
			subgid:     "bob:100000:65536\n",
			wantSubuid: "alice:100000:65536\n",
			wantSubgid: "bob:100000:65536\nalice:165536:65536\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			subuid := filepath.Join(dir, "subuid")
			subgid := filepath.Join(dir, "subgid")

			for path, data := range map[string]string{subuid: tt.subuid, subgid: tt.subgid} {
				err := os.WriteFile(path, []byte(data), 0o600)
				if err != nil {
					t.Fatal(err)
				}
			}

			script := strings.NewReplacer("/etc/subuid /etc/subgid", subuid+" "+subgid, "sudo tee", "tee").Replace(line)

			cmd := exec.CommandContext(t.Context(), "bash", "-euo", "pipefail", "-c", script)

			cmd.Env = append(os.Environ(), "USER=alice")

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", script, err, out)
			}

			for path, want := range map[string]string{subuid: tt.wantSubuid, subgid: tt.wantSubgid} {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				if string(got) != want {
					t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
				}
			}
		})
	}
}
