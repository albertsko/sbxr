package installer

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// exchange swaps the paths a and b in one atomic step, so that neither path is ever missing.
func exchange(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if err != nil {
		return fmt.Errorf("exchange %s and %s: %w", a, b, err)
	}

	return nil
}
