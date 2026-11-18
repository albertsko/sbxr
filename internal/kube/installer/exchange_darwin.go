package installer

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps the paths a and b in one atomic step. The installer runs on Linux; this lets its tests run on macOS.
func exchange(a, b string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_SWAP)
	if err != nil {
		return fmt.Errorf("exchange %s and %s: %w", a, b, err)
	}

	return nil
}
