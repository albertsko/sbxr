package image

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// CloneRoot creates dst as a private copy of the image root, so a run can change it without changing the cache.
// On macOS it is an APFS clone, which needs dst on the same APFS volume as the cache.
func (img *Image) CloneRoot(dst string) error {
	err := unix.Clonefile(img.Root, dst, unix.CLONE_NOFOLLOW)
	if err != nil {
		return fmt.Errorf("clone image root to %s: %w", dst, err)
	}

	return img.relink(dst)
}

// MountRoot is not available on macOS, which uses CloneRoot.
func (img *Image) MountRoot(string, string) (func() error, error) {
	return func() error { return nil }, errors.New("overlay roots need Linux")
}
