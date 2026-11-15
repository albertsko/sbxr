package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecvpErrors(t *testing.T) {
	denied, missing := t.TempDir(), filepath.Join(t.TempDir(), "missing")

	if err := os.WriteFile(filepath.Join(denied, "prog"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		argv []string
		path string
		want error
		code int
	}{
		{name: "empty command", argv: []string{""}, path: denied, want: unix.ENOENT, code: 127},
		{name: "not executable before missing", argv: []string{"prog"}, path: denied + ":" + missing, want: unix.EACCES, code: 126},
		{name: "missing everywhere", argv: []string{"nope"}, path: denied + ":" + missing, want: unix.ENOENT, code: 127},
		{name: "path to a missing file", argv: []string{"/nonexistent/prog"}, path: denied, want: unix.ENOENT, code: 127},
		{name: "path to a file that is not executable", argv: []string{filepath.Join(denied, "prog")}, path: missing, want: unix.EACCES, code: 126},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)

			err := execvp(tt.argv)
			if !errors.Is(err, tt.want) || exitCode(err) != tt.code {
				t.Errorf("execvp(%q) = %v (exit %d), want %v (exit %d)", tt.argv, err, exitCode(err), tt.want, tt.code)
			}
		})
	}
}
