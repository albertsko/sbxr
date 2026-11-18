package main

import "github.com/albertsko/sbxr/internal/cli"

// isolated keeps the backend: on macOS, image ownership lives in xattrs and each run root is an APFS clone.
func isolated(b cli.Backend) cli.Backend { return b }
