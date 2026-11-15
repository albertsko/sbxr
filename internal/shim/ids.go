//go:build linux

package shim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/albertsko/sbxr/internal/subid"
)

// idPool hands out the host ids of pod VMs. Each pod VM runs as root of its own user namespace, which maps
// subid.PerPod ids to a slot of the sbxr range in /etc/subuid and /etc/subgid, which no other user and no kubelet pod uses.
// The claims live in dir on tmpfs, so a reboot releases them with the pods.
type idPool struct {
	r   subid.Range
	dir string
}

const idsDir = "/run/sbxr/ids"

// loadIDPool reads the sbxr range that the installer reserved.
func loadIDPool(etc, dir string) (idPool, error) {
	subuid, err1 := os.ReadFile(filepath.Join(etc, "subuid"))
	subgid, err2 := os.ReadFile(filepath.Join(etc, "subgid"))

	if err := errors.Join(err1, err2); err != nil {
		return idPool{}, fmt.Errorf("read the sbxr id range: %w", err)
	}

	r, err := subid.Find(string(subuid), string(subgid), subid.Name)
	if err != nil {
		return idPool{}, fmt.Errorf("the sbxr id range is missing, reinstall sbxr on this node: %w", err)
	}

	return idPool{r: r, dir: dir}, nil
}

// allocate claims a free slot for the sandbox and returns its first id.
func (p idPool) allocate(sandboxID string) (int, error) {
	err := os.MkdirAll(p.dir, 0o700)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", p.dir, err)
	}

	for slot := range p.r.Count / subid.PerPod {
		f, err := os.OpenFile(filepath.Join(p.dir, strconv.Itoa(slot)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

		return p.r.First + slot*subid.PerPod, nil
	}

	return 0, errors.New("no free id range for another pod VM")
}

// release frees the slot that starts at first.
func (p idPool) release(first int) {
	slot := (first - p.r.First) / subid.PerPod
	if first < p.r.First || slot >= p.r.Count/subid.PerPod {
		return
	}

	_ = os.Remove(filepath.Join(p.dir, strconv.Itoa(slot)))
}
