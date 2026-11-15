package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/ansi"
)

// stdoutGuard points os.Stdout at a non-terminal while fang renders help and errors.
//
// fang 2.0.1 queries the terminal background color whenever os.Stdout is a terminal,
// which waits seconds on terminals that never answer (fang issue 104).
// Commands write to the real stdout through cobra, and run restores os.Stdout before any workload runs.
type stdoutGuard struct {
	real, null *os.File
}

func hideStdout() (*stdoutGuard, error) {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", os.DevNull, err)
	}

	g := &stdoutGuard{real: os.Stdout, null: null}
	g.hide()

	return g, nil
}

func (g *stdoutGuard) hide()    { os.Stdout = g.null }
func (g *stdoutGuard) restore() { os.Stdout = g.real }

// colorFile writes to its file and drops all ANSI codes when NO_COLOR is set.
// colorprofile drops only colors for NO_COLOR and keeps bold text, but sbxr promises plain output.
// It stays a file, so that terminal detection still works.
type colorFile struct{ *os.File }

func (c colorFile) Write(p []byte) (int, error) {
	if os.Getenv("NO_COLOR") == "" {
		return c.File.Write(p) //nolint:wrapcheck // This is a plain write.
	}

	_, err := io.WriteString(c.File, ansi.Strip(string(p)))
	if err != nil {
		return 0, err //nolint:wrapcheck // This is a plain write.
	}

	return len(p), nil
}
