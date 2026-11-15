//go:build !linux

// Command sbxr-mount is the guest mount helper for -v (see internal/guestmount). It runs only in a Linux guest.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sbxr-mount runs only in a Linux guest")
	os.Exit(125)
}
