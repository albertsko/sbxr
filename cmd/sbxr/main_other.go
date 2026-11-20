//go:build !linux

package main

// platformMain has nothing to do outside Linux.
func platformMain([]string) (int, bool) { return 0, false }
