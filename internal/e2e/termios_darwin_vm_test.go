//go:build vm && darwin

package e2e

import "golang.org/x/sys/unix"

const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
	// unreadBytes is FIONREAD, which golang.org/x/sys/unix does not define on darwin.
	unreadBytes = 0x4004667f
)
