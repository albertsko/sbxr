// Package cli is the sbxr command line: cobra commands rendered by fang, one-line errors, and exit codes.
package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	charmlog "charm.land/log/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/albertsko/sbxr/internal/image"
	"github.com/albertsko/sbxr/internal/preflight"
	"github.com/albertsko/sbxr/internal/run"
	"github.com/albertsko/sbxr/internal/spec"
)

// Exit codes of commands other than run.
const (
	exitFailure = 1
	exitUsage   = 2
)

const usage = `sbxr runs an OCI image as a small virtual machine.

Usage:
  sbxr run [flags] IMAGE [CMD [ARG...]]
  sbxr doctor [--json]
  sbxr prune [--force]

Examples:
  sbxr run alpine echo hi
  sbxr run -it alpine sh
  sbxr run --net alpine wget -qO- https://example.com
  sbxr run -v ./src:/src -w /src golang:1 go test ./...

Run 'sbxr --help' for all commands and flags.
`

// Backend provides the parts of sbxr that differ between builds and platforms.
type Backend struct {
	Checks func(cacheDir string) []preflight.Check
	Images func(cacheDir string) (image.Store, error)
	// Isolate, when set, runs sbxr with args in an isolated process and returns its exit code.
	// The run and prune commands then do their work through it.
	Isolate func(args []string, stdin, stdout, stderr *os.File) (int, error)
	// VMEnv is added to the environment of the VM process.
	VMEnv []string
}

// exitError ends the command with code. A nil err prints nothing.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit code %d", e.code)
	}

	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

type app struct {
	backend Backend
	args    []string
	stdout  *stdoutGuard
	log     *slog.Logger
	level   charmlog.Level
	debug   bool
}

// Main runs the sbxr command line with args and returns the process exit code.
func Main(args []string, version string, b Backend) int {
	// Without a terminal on stdout, no output has colors, including stderr.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		_ = os.Setenv("NO_COLOR", "1") // Setenv fails only on invalid names.
	}

	stdout, err := hideStdout()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbxr: %v\n", err)

		return exitFailure
	}
	defer stdout.restore()

	a := &app{backend: b, args: args, stdout: stdout, log: slog.New(slog.DiscardHandler), level: charmlog.WarnLevel, debug: false}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(colorFile{stdout.real})
	root.SetErr(colorFile{os.Stderr})

	err = fang.Execute(context.Background(), root,
		fang.WithVersion(version),
		fang.WithColorSchemeFunc(fang.AnsiColorScheme),
		fang.WithErrorHandler(printError),
		fang.WithoutManpage(),
	)

	var exit *exitError

	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	default:
		return exitFailure
	}
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "sbxr",
		Short: "Run OCI images as small virtual machines",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(cmd, exitUsage, fmt.Errorf("unknown command %q", args[0]))
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), usage)

			return err //nolint:wrapcheck // A failed usage write needs no context.
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return a.setup(cmd)
		},
	}

	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		code := exitUsage
		if cmd.Name() == "run" {
			code = run.ExitSetup
		}

		return usageError(cmd, code, err)
	})

	flags := root.PersistentFlags()
	flags.BoolVar(&a.debug, "debug", false, "Show debug logs")
	flags.Var(noColor{}, "no-color", "Disable colors")
	flags.Lookup("no-color").NoOptDefVal = "true"

	root.AddCommand(a.runCommand(), a.doctorCommand(), a.pruneCommand())

	return root
}

// setup configures logging from --debug and SBXR_LOG_LEVEL.
func (a *app) setup(cmd *cobra.Command) error {
	raw := cmp.Or(os.Getenv("SBXR_LOG_LEVEL"), "warn")
	if a.debug {
		raw = "debug"
	}

	level, err := charmlog.ParseLevel(raw)
	if err != nil {
		return failure(cmd, &spec.Error{Source: "SBXR_LOG_LEVEL", Value: raw, Problem: "unknown level", Fix: "use debug, info, warn, or error"})
	}

	a.level = level
	logger := charmlog.NewWithOptions(cmd.ErrOrStderr(), charmlog.Options{Level: level})
	a.log = slog.New(logger)

	return nil
}

// failure ends cmd with an error. The run command fails with ExitSetup, the others with exit code 1.
func failure(cmd *cobra.Command, err error) error {
	if cmd.Name() == "run" {
		return &exitError{code: run.ExitSetup, err: err}
	}

	return &exitError{code: exitFailure, err: err}
}

func usageError(cmd *cobra.Command, code int, err error) error {
	return &exitError{code: code, err: fmt.Errorf("%w; see '%s --help'", err, cmd.CommandPath())}
}

// noArgs rejects arguments as a usage error.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(cmd, exitUsage, fmt.Errorf("unexpected argument %q", args[0]))
	}

	return nil
}

// printError prints err as one line on w. fang passes a writer that drops colors when stderr is not a terminal.
func printError(w io.Writer, _ fang.Styles, err error) {
	var exit *exitError
	if errors.As(err, &exit) && exit.err == nil {
		return
	}

	prefix := lipgloss.NewStyle().Foreground(lipgloss.Red).Bold(true).Render("sbxr:")
	line := strings.ReplaceAll(err.Error(), "\n", " ")

	fmt.Fprintln(w, prefix, line) //nolint:errcheck // Nothing is left to report a failed error write to.
}

// noColor is the --no-color flag. It sets NO_COLOR, which every renderer reads.
type noColor struct{}

func (noColor) String() string   { return "false" }
func (noColor) Type() string     { return "bool" }
func (noColor) IsBoolFlag() bool { return true }

func (noColor) Set(v string) error {
	if v != "true" {
		return nil
	}

	return os.Setenv("NO_COLOR", "1") //nolint:wrapcheck // Setenv fails only on invalid names.
}

func cacheDir() (string, error) {
	if dir := os.Getenv("SBXR_CACHE_DIR"); dir != "" {
		return dir, nil
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		return "", &spec.Error{Source: "SBXR_CACHE_DIR", Problem: "not set, and the user cache directory is unknown", Fix: err.Error()}
	}

	return filepath.Join(dir, "sbxr"), nil
}
