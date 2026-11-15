//go:build linux

package shim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Each pod VM runs as root of its own user namespace, which maps idRange ids to a host range that no other pod uses.
const (
	idBase  = 1 << 30
	idRange = 1 << 16
	idSlots = 1 << 14
	idsDir  = "/run/sbxr/ids"
)

// allocateIDs claims a free host id range for the sandbox and returns its first id.
// The claims live on tmpfs, so a reboot releases them with the pods.
func allocateIDs(sandboxID string) (int, error) {
	err := os.MkdirAll(idsDir, 0o700)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", idsDir, err)
	}

	for slot := range idSlots {
		path := filepath.Join(idsDir, strconv.Itoa(slot))

		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}

		if err != nil {
			return 0, fmt.Errorf("claim id range: %w", err)
		}

		_, err = f.WriteString(sandboxID)

		err = errors.Join(err, f.Close())
		if err != nil {
			return 0, fmt.Errorf("claim id range: %w", err)
		}

		return idBase + slot*idRange, nil
	}

	return 0, errors.New("no free id range for another pod VM")
}

// releaseIDs frees the range that starts at base.
func releaseIDs(base int) {
	if base < idBase {
		return
	}

	_ = os.Remove(filepath.Join(idsDir, strconv.Itoa((base-idBase)/idRange)))
}
