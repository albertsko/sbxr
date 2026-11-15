// Package preflight holds the environment checks shared by `sbxr doctor` and `sbxr run`.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/albertsko/sbxr/internal/hv"
	"github.com/albertsko/sbxr/internal/spec"
	"github.com/albertsko/sbxr/internal/vm/libkrun"
)

// SchemaVersion is the version of the `sbxr doctor --json` output.
const SchemaVersion = 1

// Status is the outcome of one check.
type Status string

// Check outcomes.
const (
	Pass Status = "pass"
	Fail Status = "fail"
)

// Result is the outcome of one check. Fix is set only when the check fails.
type Result struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Check inspects one part of the environment.
type Check func(ctx context.Context) Result

// Report is the outcome of all checks, in the `sbxr doctor --json` shape.
type Report struct {
	SchemaVersion int      `json:"schemaVersion"`
	OK            bool     `json:"ok"`
	Checks        []Result `json:"checks"`
}

// Run runs every check in order.
func Run(ctx context.Context, checks []Check) Report {
	r := Report{SchemaVersion: SchemaVersion, OK: true, Checks: make([]Result, 0, len(checks))}

	for _, check := range checks {
		res := check(ctx)
		r.OK = r.OK && res.Status == Pass
		r.Checks = append(r.Checks, res)
	}

	return r
}

// Err returns the first failed check as a user error, or nil when every check passed.
func (r Report) Err() error {
	failed := r.Failures()
	if len(failed) == 0 {
		return nil
	}

	return &spec.Error{Source: "preflight", Value: failed[0].ID, Problem: failed[0].Detail, Fix: failed[0].Fix}
}

// Fixes returns the fix of each failed check, without repeats.
func (r Report) Fixes() []string {
	var fixes []string

	for _, res := range r.Failures() {
		if !slices.Contains(fixes, res.Fix) {
			fixes = append(fixes, res.Fix)
		}
	}

	return fixes
}

// Failures returns the failed checks.
func (r Report) Failures() []Result {
	var failed []Result

	for _, res := range r.Checks {
		if res.Status == Fail {
			failed = append(failed, res)
		}
	}

	return failed
}

// Passed returns a passing result.
func Passed(id, detail string) Result {
	return Result{ID: id, Status: Pass, Detail: detail, Fix: ""}
}

// Failed returns a failing result with a fix command or hint.
func Failed(id, detail, fix string) Result {
	return Result{ID: id, Status: Fail, Detail: detail, Fix: fix}
}

// Checks returns the doctor checks of this platform in the PRD order.
// libDir is the SBXR_LIB_DIR value, empty for the default library directories.
func Checks(cacheDir, libDir string) []Check {
	checks := []Check{Hypervisor, Libkrun(libDir), Libkrunfw(libDir)}
	checks = append(checks, platformChecks...)

	return append(checks, Cache(cacheDir))
}

// Hypervisor checks that the machine supports hardware virtualization.
func Hypervisor(context.Context) Result {
	const id = "hypervisor"

	err := hv.Check()
	if err != nil {
		return Failed(id, err.Error(), hypervisorFix(err))
	}

	return Passed(id, hypervisorDetail)
}

// Libkrun checks that libkrun loads from the library directory and exports every required symbol.
func Libkrun(libDir string) Check {
	return func(context.Context) Result {
		const id = "libkrun"

		dir, err := libkrun.LibDir(libDir)
		if err == nil {
			err = libkrun.CheckLibkrun(dir)
		}

		if err != nil {
			return Failed(id, err.Error(), libFix(libDir))
		}

		return Passed(id, "libkrun loads from "+dir)
	}
}

// Libkrunfw checks that libkrunfw loads from the library directory and is new enough.
func Libkrunfw(libDir string) Check {
	return func(context.Context) Result {
		const id = "libkrunfw"

		dir, err := libkrun.LibDir(libDir)
		if err != nil {
			return Failed(id, err.Error(), libFix(libDir))
		}

		version, err := libkrun.CheckLibkrunfw(dir)
		if err != nil {
			return Failed(id, err.Error(), krunfwFix(libDir))
		}

		return Passed(id, "libkrunfw "+version+" loads from "+dir)
	}
}

func libFix(libDir string) string {
	if libDir != "" {
		return "set SBXR_LIB_DIR to the directory that holds libkrun and libkrunfw"
	}

	return libkrunInstall
}

func krunfwFix(libDir string) string {
	if libDir != "" {
		return "install libkrunfw 5.6.4 or later into SBXR_LIB_DIR"
	}

	return krunfwInstall
}

// Cache checks that dir is writable.
func Cache(dir string) Check {
	return func(context.Context) Result {
		const id = "cache"

		err := writable(dir)
		if err != nil {
			return Failed(id, err.Error(), "set SBXR_CACHE_DIR to a writable directory")
		}

		return Passed(id, dir+" is writable")
	}
}

func writable(dir string) error {
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fmt.Errorf("write to cache directory: %w", err)
	}

	err = errors.Join(f.Close(), os.Remove(f.Name()))
	if err != nil {
		return fmt.Errorf("remove test file: %w", err)
	}

	return nil
}
