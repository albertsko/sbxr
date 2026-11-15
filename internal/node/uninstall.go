package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Uninstall removes the label and annotation, then every sbxr drop-in, restarts containerd, and removes the bundle.
// It keeps the bundle while containerd has not restarted or an sbxr container still exists, records why on the node, and returns an error.
func (in *Installer) Uninstall(ctx context.Context) error {
	unlock, err := lock(in.HostRoot + lockFile)
	if err != nil {
		return err
	}
	defer unlock()

	err = in.setStatus(ctx, false, nil)
	if err != nil {
		return err
	}

	err = in.removeDropIns(ctx)
	if err != nil {
		return in.fail(ctx, err)
	}

	containers, err := in.containers()
	if err != nil {
		return in.fail(ctx, err)
	}

	if len(containers) > 0 {
		return in.fail(ctx, fmt.Errorf("%d sbxr containers still exist (%s); delete the sbxr pods on this node", len(containers), strings.Join(containers, ", ")))
	}

	// Keep verifyAfter: the kubelet may still post the handler of the removed drop-in, and a reinstall must not trust it.
	err = errors.Join(
		os.RemoveAll(in.HostRoot+Prefix), os.RemoveAll(in.HostRoot+Prefix+".new"),
		os.RemoveAll(in.HostRoot+filepath.Dir(RuntimeRoot)),
	)
	if err != nil {
		return in.fail(ctx, fmt.Errorf("remove files: %w", err))
	}

	err = in.setStatus(ctx, false, nil)
	if err != nil {
		return err
	}

	in.Log.Info("node clean")

	return nil
}

// removeDropIns removes the drop-in of every known distribution and restarts containerd.
// The pending restart mark lives outside the bundle, so a retry, even after a failed reinstall, still restarts.
func (in *Installer) removeDropIns(ctx context.Context) error {
	for _, d := range distros {
		path := in.HostRoot + d.dropInFile()
		if _, err := os.Stat(path); err != nil {
			continue
		}

		// Record the unit while it may still be active. A retry restarts it even when containerd is down.
		if unit, pending := in.pendingRestart(); !pending || unit == "" {
			_, active, _ := in.detect(ctx)

			err := in.markRestart(active)
			if err != nil {
				return err
			}
		}

		err := os.Remove(path)
		if err != nil {
			return fmt.Errorf("remove drop-in: %w", err)
		}

		in.Log.Info("drop-in removed", "path", d.dropInFile())
	}

	unit, pending := in.pendingRestart()
	if !pending {
		return nil
	}

	if unit == "" {
		var err error

		_, unit, err = in.detect(ctx)
		if err != nil {
			return fmt.Errorf("drop-in removed, but containerd cannot restart: %w", err)
		}
	}

	return in.restart(ctx, unit, false)
}

// containers lists the containers that crun still has state for under RuntimeRoot/<namespace>/<id>.
// This covers pod sandboxes too, which run as normal processes.
// Every released drop-in sets RuntimeRoot, so no sbxr container has state under containerd's default runc root.
func (in *Installer) containers() ([]string, error) {
	namespaces, err := os.ReadDir(in.HostRoot + RuntimeRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("list sbxr containers: %w", err)
	}

	var ids []string

	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}

		entries, err := os.ReadDir(filepath.Join(in.HostRoot+RuntimeRoot, ns.Name()))
		if err != nil {
			return nil, fmt.Errorf("list sbxr containers: %w", err)
		}

		for _, e := range entries {
			// crun keeps its own .cache and .empty-directory there.
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				ids = append(ids, e.Name())
			}
		}
	}

	return ids, nil
}

// fail records the uninstall failure on the node and returns it, so that the pod retries.
func (in *Installer) fail(ctx context.Context, err error) error {
	reason := "uninstall: " + err.Error()
	in.Log.Warn("uninstall incomplete", "reason", reason)

	return errors.Join(err, in.setStatus(ctx, false, &reason))
}
