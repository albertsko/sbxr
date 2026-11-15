// Package node installs and removes the phase 1 runtime on one Kubernetes node (PRD "Kubernetes contract").
//
// It runs in a privileged DaemonSet pod with the host root filesystem mounted and the host PID namespace.
// Host commands run in the host mount namespace, and the installer's own Node object holds the result.
package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Node label, annotation, handler, and install directory from the PRD.
const (
	ReadyLabel          = "sbxr.dev/ready"
	PreflightAnnotation = "sbxr.dev/preflight"
	Handler             = "sbxr"
	Prefix              = "/opt/sbxr"
)

// Host paths besides the bundle.
const (
	// lockFile serializes installer pods on one node. It is never removed, so every pod locks the same inode.
	lockFile = "/run/sbxr.lock"
	// RuntimeRoot is the crun state directory of handler sbxr in phase 1. Each container of an sbxr pod has state there while it exists.
	RuntimeRoot = "/run/sbxr/runc"
	// PodsRoot holds a directory for each phase 2 pod while it exists.
	PodsRoot = "/run/sbxr/pods"
	// ShimLink makes the phase 2 shim visible to containerd. containerd's shim sandboxer finds a shim only by its name,
	// in PATH or next to the containerd binary, and ignores runtime_path.
	ShimLink = "/usr/local/bin/containerd-shim-sbxr-v1"
	shimPath = Prefix + "/bin/containerd-shim-sbxr-v1"
	// stateDir holds installer state that must survive a bundle replacement.
	stateDir = "/var/lib/sbxr"
	// restartPending exists from a drop-in change until containerd restarts successfully. It holds the unit to restart.
	restartPending = stateDir + "/restart-pending"
	// verifyAfter holds the time after which a node status can show the restarted containerd.
	// It survives retries and goes only after the node is labeled ready.
	verifyAfter = stateDir + "/verify-after"
)

// Installer holds what Install and Uninstall need.
type Installer struct {
	API API
	// Host runs a command in the host mount namespace.
	Host func(ctx context.Context, args ...string) (string, error)
	// HostRoot is where the host root filesystem is mounted.
	HostRoot string
	// Bundle is the node bundle directory with its SHA256SUMS file.
	Bundle string
	// Phase selects the runtime: 1 runs crun with libkrun per container, 2 runs the sbxr shim with one VM per pod.
	Phase int
	// Poll and Timeout bound the wait for the node to report handler sbxr.
	Poll    time.Duration
	Timeout time.Duration
	Log     *slog.Logger
}

// rejection is a reason why this node cannot run sbxr.
type rejection struct{ error }

// Install runs the installer steps of PRD "Kubernetes contract" on this node.
// A node that fails preflight or verification gets the reason in its annotation, not an error.
// An error means that the installer itself failed and should be retried.
func (in *Installer) Install(ctx context.Context) error {
	unlock, err := lock(in.HostRoot + lockFile)
	if err != nil {
		return err
	}
	defer unlock()

	// A pending restart means that an earlier run changed containerd's config, so this is no first load.
	_, pending := in.pendingRestart()

	// A failed rollback can leave containerd stopped, so preflight would find no active unit.
	err = in.finishPendingRestart(ctx)
	if err != nil {
		return errors.Join(err, in.reject(ctx, "pending restart: "+err.Error()))
	}

	d, unit, dropIn, err := in.preflight(ctx)
	if reject, ok := errors.AsType[rejection](err); ok {
		return in.reject(ctx, "preflight: "+reject.Error())
	}

	if err != nil {
		return err
	}

	in.Log.Info("preflight passed", "distribution", d.name, "unit", unit)

	err = installBundle(in.Bundle, in.HostRoot+Prefix)
	if err != nil {
		return err
	}

	out, err := in.Host(ctx, Prefix+"/bin/krun", "--version")
	if err != nil || !strings.Contains(out, "+LIBKRUN") {
		return in.reject(ctx, fmt.Sprintf("bundle: krun --version on the host: %v: %s", err, out))
	}

	if in.Phase == 2 {
		err = linkShim(in.HostRoot + ShimLink)
		if err != nil {
			return err
		}
	}

	return in.configure(ctx, in.HostRoot+d.dropInFile(), unit, dropIn, pending)
}

// configure publishes the drop-in, restarts the unit when needed, verifies the handler, and labels the node.
// Any failure after publishing, except an unanswered API server during verification, restores the previous drop-in (or removes it), restarts again, and rejects the node.
// wasPending says whether a restart was pending when this run started.
func (in *Installer) configure(ctx context.Context, path, unit string, dropIn []byte, wasPending bool) error {
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read drop-in: %w", err)
	}

	changed := err != nil || !bytes.Equal(previous, dropIn)
	if !changed {
		// The drop-in is already ours, so a rollback removes it.
		previous = nil
	}

	err = in.apply(ctx, path, unit, dropIn, changed, wasPending)
	if err == nil {
		in.Log.Info("node ready")

		return nil
	}

	if _, ok := errors.AsType[apiError](err); ok {
		// Nothing shows that the drop-in is wrong. The pod retries.
		return errors.Join(err, in.reject(ctx, "verify: "+err.Error()))
	}

	in.Log.Warn("rolling back the drop-in", "error", err)

	rollback := in.markRestart(unit)
	if rollback == nil {
		rollback = restoreFile(path, previous)
	}

	if rollback == nil {
		rollback = in.restart(ctx, unit, false)
	}

	if rollback != nil {
		rollback = fmt.Errorf("roll back: %w", rollback)
	}

	reject := in.reject(ctx, "verify: "+errors.Join(err, rollback).Error())

	if _, ok := errors.AsType[unverified](err); ok && rollback == nil {
		return reject
	}

	return errors.Join(err, rollback, reject)
}

// apiError is an API server failure. It says nothing about the node.
type apiError struct{ error }

// unverified means that containerd did not restart or the node does not serve the handler.
type unverified struct{ error }

// apply publishes the drop-in when it changed, restarts the unit when a restart is pending, verifies, and labels the node.
func (in *Installer) apply(ctx context.Context, path, unit string, dropIn []byte, changed, wasPending bool) error {
	// This is a first load only when no sbxr drop-in is on disk and no earlier run left a restart pending.
	// Otherwise a containerd from before this restart may have served the handler, and the kubelet may still post it.
	_, err := os.Stat(path)
	firstLoad := errors.Is(err, fs.ErrNotExist) && !wasPending

	if changed {
		err = in.markRestart(unit)
		if err == nil {
			err = writeFile(path, dropIn)
		}

		if err != nil {
			return err
		}
	}

	// A pending restart also covers a drop-in that an earlier installer wrote but never loaded.
	if _, pending := in.pendingRestart(); pending {
		in.Log.Info("restarting", "unit", unit)

		err = in.restart(ctx, unit, firstLoad)
		if err != nil {
			return err
		}
	}

	cutoff, err := in.verifyCutoff()
	if err != nil {
		return err
	}

	err = in.waitHandler(ctx, cutoff)
	if err != nil {
		return err
	}

	err = in.setStatus(ctx, true, new("ok"))
	if err != nil {
		// The node serves sbxr, so a retry only needs to label it.
		return apiError{err}
	}

	// A leftover cutoff only asks the next run for a status newer than this restart, which every status is.
	_ = os.Remove(in.HostRoot + verifyAfter)

	return nil
}

// pendingRestart returns the unit of a pending containerd restart, which may be empty, and whether one is pending.
func (in *Installer) pendingRestart() (string, bool) {
	unit, err := os.ReadFile(in.HostRoot + restartPending)

	return strings.TrimSpace(string(unit)), err == nil
}

// markRestart records that containerd, under unit, must restart before the node can be trusted. A successful restart removes the mark.
func (in *Installer) markRestart(unit string) error {
	return writeFile(in.HostRoot+restartPending, []byte(unit+"\n"))
}

// finishPendingRestart restarts the unit that an earlier run marked and did not restart. It needs no API server.
func (in *Installer) finishPendingRestart(ctx context.Context) error {
	unit, pending := in.pendingRestart()
	if !pending || unit == "" {
		return nil
	}

	in.Log.Info("finishing a pending restart", "unit", unit)

	return in.restart(ctx, unit, false)
}

// verifyCutoff returns the persisted verification cutoff, or the zero time when no restart awaits verification.
func (in *Installer) verifyCutoff() (time.Time, error) {
	data, err := os.ReadFile(in.HostRoot + verifyAfter)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}

	if err != nil {
		return time.Time{}, fmt.Errorf("read verification cutoff: %w", err)
	}

	cutoff, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, fmt.Errorf("read verification cutoff: %w", err)
	}

	return cutoff, nil
}

// reject records reason on the node and leaves it unlabeled.
func (in *Installer) reject(ctx context.Context, reason string) error {
	in.Log.Warn("node not ready", "reason", reason)

	return in.setStatus(ctx, false, &reason)
}

// setStatus sets or removes the ready label, and sets the preflight annotation or removes it when nil.
func (in *Installer) setStatus(ctx context.Context, ready bool, annotation *string) error {
	var label *string
	if ready {
		label = new("true")
	}

	err := in.API.Patch(ctx, map[string]*string{ReadyLabel: label}, map[string]*string{PreflightAnnotation: annotation})
	if err != nil {
		return fmt.Errorf("update node status: %w", err)
	}

	return nil
}

func (in *Installer) detect(ctx context.Context) (distro, string, error) {
	out, err := in.Host(ctx, "systemctl", "list-units", "--type=service", "--state=active", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return distro{}, "", fmt.Errorf("list systemd units: %w", err)
	}

	var active []string

	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 {
			active = append(active, f[0])
		}
	}

	d, unit, err := detect(active)
	if err != nil {
		return distro{}, "", rejection{err}
	}

	return d, unit, nil
}

// preflight returns the distribution, its unit, and the drop-in content. A rejection error means the node is not supported.
func (in *Installer) preflight(ctx context.Context) (distro, string, []byte, error) {
	d, unit, err := in.detect(ctx)
	if err != nil {
		return distro{}, "", nil, err
	}

	n, err := in.API.Node(ctx)
	if err != nil {
		return distro{}, "", nil, fmt.Errorf("read node: %w", err)
	}

	config, err := os.ReadFile(in.HostRoot + d.config)
	if err != nil {
		return distro{}, "", nil, rejection{fmt.Errorf("read containerd config: %w", err)}
	}

	_, kvmErr := in.Host(ctx, "test", "-c", "/dev/kvm")
	_, dbusErr := in.Host(ctx, "test", "-S", "/run/dbus/system_bus_socket")
	_, tunErr := in.Host(ctx, "test", "-c", "/dev/net/tun")
	glibc, _ := in.Host(ctx, "getconf", "GNU_LIBC_VERSION")
	kernel, _ := in.Host(ctx, "uname", "-r")

	dropIn, err := preflight(d, facts{
		kvm:     kvmErr == nil,
		glibc:   glibc,
		runtime: n.RuntimeVersion,
		config:  string(config),
		systemd: runcSystemdCgroup(append([]string{string(config)}, in.imported(string(config), d)...)...),
		dbus:    dbusErr == nil,
		kernel:  kernel,
		tun:     tunErr == nil,
	}, in.Phase)
	if err != nil {
		return distro{}, "", nil, rejection{err}
	}

	return d, unit, dropIn, nil
}

// imported returns the content of every file that the main config imports, in import order, except the sbxr drop-in.
func (in *Installer) imported(config string, d distro) []string {
	var files []string

	for _, p := range importPatterns(config, d.config) {
		matches, _ := filepath.Glob(in.HostRoot + p)
		for _, m := range matches {
			if m == in.HostRoot+d.dropInFile() {
				continue
			}

			data, err := os.ReadFile(m)
			if err == nil {
				files = append(files, string(data))
			}
		}
	}

	return files
}

// restart restarts unit and checks that it is active. Then it records the verification cutoff and clears the pending restart.
// A node status counts only when the kubelet posted it after the cutoff.
// With firstLoad, no sbxr drop-in existed before the restart, so only the restarted containerd can list the handler and the cutoff is the restart.
// Otherwise the kubelet may still post a cached handler from before the restart, so the cutoff is runtimeRefresh later.
// The cutoff never moves earlier and survives uninstall, so a reinstall right after an uninstall still waits for the barrier.
func (in *Installer) restart(ctx context.Context, unit string, firstLoad bool) error {
	out, err := in.Host(ctx, "systemctl", "restart", unit)
	if err != nil {
		return unverified{fmt.Errorf("restart %s: %w: %s", unit, err, out)}
	}

	out, err = in.Host(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return unverified{fmt.Errorf("%s is not active after the restart: %w: %s", unit, err, strings.TrimSpace(out))}
	}

	cutoff := time.Now()
	if !firstLoad {
		cutoff = cutoff.Add(runtimeRefresh)
	}

	// A later restart never moves a persisted cutoff earlier.
	persisted, err := in.verifyCutoff()
	if err != nil {
		return err
	}

	if persisted.After(cutoff) {
		cutoff = persisted
	}

	err = writeFile(in.HostRoot+verifyAfter, []byte(cutoff.Format(time.RFC3339Nano)+"\n"))
	if err != nil {
		return err
	}

	err = os.Remove(in.HostRoot + restartPending)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear pending restart: %w", err)
	}

	return nil
}

// runtimeRefresh bounds how old the kubelet's cached runtime state can be when it posts a node status.
const runtimeRefresh = 10 * time.Second

// waitHandler waits until the node reports handler sbxr in a status posted after fresh.
// It returns an apiError when the last poll failed, and an unverified error when the last poll showed no fresh handler.
func (in *Installer) waitHandler(ctx context.Context, fresh time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()

	var last error

	for {
		n, err := in.API.Node(ctx)

		switch {
		case err != nil:
			last = apiError{fmt.Errorf("read node: %w", err)}
		case !n.Heartbeat.After(fresh):
			last = unverified{fmt.Errorf("no node status after %s", fresh.Format(time.RFC3339))}
		case !slices.Contains(n.RuntimeHandlers, Handler):
			last = unverified{fmt.Errorf("node does not report runtime handler %s", Handler)}
		default:
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("after %s: %w", in.Timeout, last)
		case <-time.After(in.Poll):
		}
	}
}
