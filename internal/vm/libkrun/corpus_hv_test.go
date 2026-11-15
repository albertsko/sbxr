//go:build hv && (darwin || linux)

package libkrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/albertsko/sbxr/internal/krunconfig"
)

// corpusDir is the seed corpus of krunconfig.FuzzMarshal.
const corpusDir = "../../krunconfig/testdata/fuzz/FuzzMarshal"

// TestVMRunsTheFuzzSeedCorpus boots each seed that Marshal accepts through the real guest init.
// The workload prints what it received, which must match the config.
func TestVMRunsTheFuzzSeedCorpus(t *testing.T) {
	t.Parallel()

	libDir := requireVM(t)
	guest := buildGuest(t)

	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		cfg, err := readSeed(filepath.Join(corpusDir, e.Name()))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}

		// The guest reports on the seed arguments, so they reach it unchanged after its own two.
		seedArgs := cfg.Args
		cfg.Args = append([]string{"/guest", "report"}, seedArgs...)

		if _, err := krunconfig.Marshal(cfg); err != nil {
			continue
		}

		t.Run(e.Name(), func(t *testing.T) {
			t.Parallel()

			out, code := runConfig(t, libDir, newRoot(t, guest, cfg), nil)
			if code != 0 {
				t.Fatalf("exit code %d, stdout %q", code, out)
			}

			var got struct {
				Args     []string
				Env      []string
				UID, GID uint32
				Cwd      string
			}

			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}

			if seedArgs == nil {
				seedArgs = []string{}
			}

			if !slices.Equal(got.Args, seedArgs) || got.UID != cfg.UID || got.GID != cfg.GID || got.Cwd != cfg.WorkingDir {
				t.Errorf("guest got args %q, uid %d, gid %d, cwd %q; want %q, %d, %d, %q",
					got.Args, got.UID, got.GID, got.Cwd, seedArgs, cfg.UID, cfg.GID, cfg.WorkingDir)
			}

			slices.Sort(got.Env)

			if !slices.Equal(got.Env, guestEnv(cfg.Env)) {
				t.Errorf("guest env %q, want %q", got.Env, guestEnv(cfg.Env))
			}
		})
	}
}

// guestEnv returns env with the HOME=/ and TERM=linux that the guest init adds when they are missing, sorted.
func guestEnv(env []string) []string {
	out := slices.Clone(env)

	for _, def := range []string{"HOME=/", "TERM=linux"} {
		name, _, _ := strings.Cut(def, "=")
		if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") }) {
			out = append(out, def)
		}
	}

	slices.Sort(out)

	return out
}

// readSeed parses a corpus file of FuzzMarshal(uid, gid uint32, workdir, env, args string).
func readSeed(path string) (krunconfig.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return krunconfig.Config{}, fmt.Errorf("read seed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 6 || lines[0] != "go test fuzz v1" {
		return krunconfig.Config{}, fmt.Errorf("want 5 values in the go test fuzz v1 format, got %q", data)
	}

	var ids [2]uint32

	for i, line := range lines[1:3] {
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(line, "uint32("), ")"), 10, 32)
		if err != nil {
			return krunconfig.Config{}, fmt.Errorf("value %d: %w", i+1, err)
		}

		ids[i] = uint32(n)
	}

	var strs [3]string

	for i, line := range lines[3:] {
		s, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(line, "string("), ")"))
		if err != nil {
			return krunconfig.Config{}, fmt.Errorf("value %d: %w", i+3, err)
		}

		strs[i] = s
	}

	list := func(s string) []string {
		if s == "" {
			return nil
		}

		return strings.Split(s, "\x00")
	}

	return krunconfig.Config{UID: ids[0], GID: ids[1], WorkingDir: strs[0], Env: list(strs[1]), Args: list(strs[2])}, nil
}
