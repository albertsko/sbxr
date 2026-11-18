//go:build !linux

package sbxrmain

// platformMain has nothing to do outside Linux.
func platformMain([]string) (int, bool) { return 0, false }
