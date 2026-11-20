package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/albertsko/sbxr/internal/preflight"
)

// The host tests run the release binary only up to the point where it would pull an image or start a VM.

// noPullImage is an image that no run in the host tests may pull.
// Nothing listens on port 1, so a run that reaches the pull fails at once instead of using the network.
const noPullImage = "127.0.0.1:1/sbxr/busybox"

// hostEnv returns a minimal environment with a private cache, plus extra.
func hostEnv(t *testing.T, extra ...string) []string {
	t.Helper()

	return append([]string{"PATH=" + os.Getenv("PATH"), "SBXR_CACHE_DIR=" + filepath.Join(t.TempDir(), "cache")}, extra...)
}

// noLibraries returns the SBXR_LIB_DIR entry of an empty directory, which makes the libkrun and libkrunfw checks fail.
func noLibraries(t *testing.T) string {
	t.Helper()

	return "SBXR_LIB_DIR=" + t.TempDir()
}

func TestHelpAndErrorsDoNotWaitForTerminalQueries(t *testing.T) {
	t.Parallel()

	bin := releaseBinary(t)

	tests := []struct {
		name     string
		args     []string
		extraEnv []string
		wantCode int
		// want is a text of the help, or the message after the styled "sbxr:" prefix of an error.
		want string
		// wantAny lists texts of which the output must contain at least one.
		wantAny []string
	}{
		{name: "help", args: []string{"--help"}, wantCode: 0, want: "doctor"},
		{name: "run help", args: []string{"run", "--help"}, wantCode: 0, want: "--allow-lan"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, want: ` unknown command "nope"`},
		{name: "bad flag", args: []string{"run", "--nope"}, wantCode: 125, want: " unknown flag: --nope"},
		{name: "bad doctor flag", args: []string{"doctor", "--nope"}, wantCode: 2, want: " unknown flag: --nope"},
		// The first failed check depends on the host: hypervisor where there is none, else libkrun.
		{name: "failed preflight", args: []string{"run", noPullImage}, extraEnv: []string{noLibraries(t)}, wantCode: 125, want: ` preflight "`, wantAny: []string{`"hypervisor": `, `"libkrun": `}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, code, elapsed := onTerminal(t, bin, hostEnv(t, tt.extraEnv...), tt.args...)
			if elapsed >= time.Second {
				t.Errorf("sbxr %q took %v, want under 1s", tt.args, elapsed)
			}

			// An error starts with the styled prefix, and the message follows as plain text.
			text := out
			if tt.wantCode != 0 {
				_, text, _ = strings.Cut(out, "sbxr:")
			}

			anyFound := len(tt.wantAny) == 0 || slices.ContainsFunc(tt.wantAny, func(s string) bool { return strings.Contains(text, s) })

			if code != tt.wantCode || !strings.Contains(text, tt.want) || !anyFound {
				t.Errorf("sbxr %q = %d, %q; want %d and output with %q and one of %q", tt.args, code, out, tt.wantCode, tt.want, tt.wantAny)
			}
		})
	}
}

func TestColorsOnlyOnTerminalWithoutNoColor(t *testing.T) {
	t.Parallel()

	bin := releaseBinary(t)
	runError := []string{"run", "-w", "app", noPullImage}

	tests := []struct {
		name     string
		extraEnv []string
		args     []string
		wantANSI bool
	}{
		{name: "error", args: runError, wantANSI: true},
		{name: "error with NO_COLOR", extraEnv: []string{"NO_COLOR=1"}, args: runError},
		{name: "error with --no-color", args: append([]string{"--no-color"}, runError...)},
		{name: "help", args: []string{"run", "--help"}, wantANSI: true},
		{name: "help with NO_COLOR", extraEnv: []string{"NO_COLOR=1"}, args: []string{"run", "--help"}},
		{name: "help with --no-color", args: []string{"run", "--help", "--no-color"}},
		{name: "doctor with --no-color", extraEnv: []string{noLibraries(t)}, args: []string{"doctor", "--no-color"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, _, _ := onTerminal(t, bin, hostEnv(t, tt.extraEnv...), tt.args...)
			if strings.Contains(out, "\x1b") != tt.wantANSI {
				t.Errorf("output %q, want ANSI codes: %v", out, tt.wantANSI)
			}
		})
	}
}

func TestErrorIsOneLine(t *testing.T) {
	t.Parallel()

	out, code, _ := onTerminal(t, releaseBinary(t), hostEnv(t, "NO_COLOR=1"), "run", "-w", "app", noPullImage)

	want := "sbxr: -w \"app\": must be absolute; for example /app\r\n"
	if code != 125 || out != want {
		t.Errorf("sbxr run -w app = %d, %q; want 125, %q", code, out, want)
	}
}

func TestNoColorsWhenStdoutIsNotATerminal(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer

	out, code, _ := startOnTerminal(t, releaseBinary(t), hostEnv(t), &stdout, nil, "run", "-w", "app", noPullImage).wait()
	if code != 125 || out != "sbxr: -w \"app\": must be absolute; for example /app\r\n" {
		t.Errorf("stderr on a terminal = %d, %q; want 125 and the error without ANSI codes", code, out)
	}
}

func TestPipedOutput(t *testing.T) {
	t.Parallel()

	bin := releaseBinary(t)

	tests := []struct {
		name       string
		args       []string
		extraEnv   []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no arguments print usage", args: nil, wantStdout: "sbxr run --net "},
		{name: "control character in argument", args: []string{"run", noPullImage, "echo", "a\x1bb"}, wantCode: 125, wantStderr: `sbxr: argument 2 "a\x1bb": control character U+001B; only tab and newline are allowed` + "\n"},
		{name: "reserved env", args: []string{"run", "-e", "KRUN_INIT=/x", noPullImage}, wantCode: 125, wantStderr: `sbxr: -e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable` + "\n"},
		{name: "publish without net", args: []string{"run", "-p", "8080:80", noPullImage}, wantCode: 125, wantStderr: `sbxr: -p "8080:80": requires --net` + "\n"},
		{name: "interactive terminal without a terminal", args: []string{"run", "-it", noPullImage, "true"}, wantCode: 125, wantStderr: `sbxr: -t: the input device is not a terminal; drop -t, or run sbxr from a terminal` + "\n"},
		{name: "bad log level", args: []string{"doctor"}, extraEnv: []string{"SBXR_LOG_LEVEL=loud"}, wantCode: 1, wantStderr: `sbxr: SBXR_LOG_LEVEL "loud": unknown level; use debug, info, warn, or error` + "\n"},
		{name: "doctor extra argument", args: []string{"doctor", "x"}, wantCode: 2, wantStderr: `sbxr: unexpected argument "x"; see 'sbxr doctor --help'` + "\n"},
		{name: "apparmor profile", args: []string{"doctor", "apparmor-profile"}, wantStdout: "profile sbxr /{usr/bin,usr/local/bin}/sbxr flags=(unconfined) {"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := sbxr(t, bin, hostEnv(t, tt.extraEnv...), tt.args...)
			if res.code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", res.code, tt.wantCode, res.stderr)
			}

			if !strings.Contains(res.stdout, tt.wantStdout) || tt.wantStdout == "" && res.stdout != "" {
				t.Errorf("stdout = %q, want %q", res.stdout, tt.wantStdout)
			}

			if res.stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", res.stderr, tt.wantStderr)
			}
		})
	}
}

// TestDoctorSetupBlock checks that a failed doctor report ends with the Linux setup block on Linux only.
// TestVMDoctorPassesWithoutSetupBlock checks the report where all checks pass.
func TestDoctorSetupBlock(t *testing.T) {
	t.Parallel()

	res := sbxr(t, releaseBinary(t), hostEnv(t, noLibraries(t)), "doctor")

	want := runtime.GOOS == "linux"
	if got := strings.HasSuffix(res.stdout, "\n"+preflight.LinuxSetup); res.code != 1 || got != want {
		t.Errorf("sbxr doctor = %d, %q; want 1, and the setup block at the end: %v", res.code, res.stdout, want)
	}
}

func TestDoctorJSON(t *testing.T) {
	t.Parallel()

	res := sbxr(t, releaseBinary(t), hostEnv(t, noLibraries(t)), "doctor", "--json")
	if res.code != 1 {
		t.Errorf("exit code = %d, want 1", res.code)
	}

	var report struct {
		SchemaVersion int  `json:"schemaVersion"`
		OK            bool `json:"ok"`
		Checks        []map[string]string
	}

	err := json.Unmarshal([]byte(res.stdout), &report)
	if err != nil {
		t.Fatalf("stdout %q: %v", res.stdout, err)
	}

	if report.SchemaVersion != 1 || report.OK || len(report.Checks) == 0 {
		t.Fatalf("report = %+v, want schema 1, not ok, with checks", report)
	}

	// Other checks may fail too, for example hypervisor on a machine without one.
	for _, c := range report.Checks {
		_, hasFix := c["fix"]
		failed := c["status"] == "fail"
		mustFail := c["id"] == "libkrun" || c["id"] == "libkrunfw"

		if hasFix != failed || mustFail && !failed || c["detail"] == "" {
			t.Errorf("check %v, want a fix exactly when it fails, a detail, and status fail for libkrun and libkrunfw", c)
		}
	}
}

// TestDoctorJSONMatchesSchema checks doctor reports against docs/doctor.schema.json.
// It also checks that the release build runs the doctor checks of docs/cli.md in order.
func TestDoctorJSONMatchesSchema(t *testing.T) {
	t.Parallel()

	bin := releaseBinary(t)

	schema, err := jsonschema.NewCompiler().Compile("../../docs/doctor.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	platformCheck := map[string]string{"darwin": "entitlement", "linux": "userns"}[runtime.GOOS]
	wantIDs := []string{"hypervisor", "libkrun", "libkrunfw", platformCheck, "cache"}

	tests := []struct {
		name    string
		environ []string
	}{
		{name: "default libraries", environ: hostEnv(t)},
		{name: "without libraries", environ: hostEnv(t, noLibraries(t))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), bin, "doctor", "--json")
			cmd.Env = tt.environ

			out, err := cmd.Output()
			if _, isExit := errors.AsType[*exec.ExitError](err); err != nil && !isExit {
				t.Fatal(err)
			}

			report, err := jsonschema.UnmarshalJSON(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("stdout %q: %v", out, err)
			}

			err = schema.Validate(report)
			if err != nil {
				t.Errorf("doctor --json %s does not match the schema: %v", out, err)
			}

			checks := report.(map[string]any)["checks"].([]any) //nolint:forcetypeassert // The schema guarantees the shape.
			ids := make([]string, 0, len(checks))

			for _, c := range checks {
				ids = append(ids, c.(map[string]any)["id"].(string)) //nolint:forcetypeassert // The schema guarantees the shape.
			}

			if !slices.Equal(ids, wantIDs) {
				t.Errorf("check IDs %q, want %q", ids, wantIDs)
			}
		})
	}
}

// result is the outcome of one sbxr process.
type result struct {
	stdout, stderr string
	code           int
}

// sbxr runs bin with stdout and stderr on pipes, for at most a minute.
func sbxr(t *testing.T, bin string, environ []string, args ...string) result {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = environ

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}

	return result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}
