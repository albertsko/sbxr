//go:build vm && linux

package e2e

import "golang.org/x/sys/unix"

const (
	getTermios  = unix.TCGETS
	setTermios  = unix.TCSETS
	unreadBytes = unix.TIOCINQ
)
