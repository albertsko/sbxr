//go:build linux

// Command containerd-shim-sbxr-v1 is the sbxr phase 2 containerd runtime, runtime type io.containerd.sbxr.v1.
package main

import (
	"os"

	"github.com/albertsko/sbxr/internal/shim"
)

func main() {
	os.Exit(shim.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
