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

	"github.com/albertsko/sbxr/internal/subid"
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
	// RetiredRoot holds bundles that a newer install replaced, while running VMs, shims, or crun still use them.
	// The installer also stages each new bundle there.
	RetiredRoot = Prefix + ".old"
	// legacyStaged is where installers before RetiredRoot staged bundles. After an interrupted exchange it held the
	// previous bundle.
	legacyStaged = Prefix + ".new"
	// ShimLink makes the phase 2 shim visible to containerd. containerd's shim sandboxer finds a shim only by its name,
	// in PATH or next to the containerd binary, and ignores runtime_path.
	ShimLink = "/usr/local/bin/containerd-shim-sbxr-v1"
	shimPath = Prefix + "/bin/containerd-shim-sbxr-v1"
	// stateDir holds installer state that must survive a bundle replacement.
	stateDir = "/var/lib/sbxr"
	// restartPending exists from a drop-in change until containerd restarts successfully. It holds the unit to restart.
	restartPending = stateDir + "/restart-pending"
	// legacyVerifyAfter is a verification cutoff of older installers, which waited for a fresh node status.
	legacyVerifyAfter = stateDir + "/verify-after"
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
	// CRI returns the runtime handlers that the CRI at a socket serves.
	CRI func(ctx context.Context, socket string) ([]string, error)
	// Poll and Timeout bound the wait for containerd to serve handler sbxr, and for the node to report its runtime.
	Poll    time.Duration
	Timeout time.Duration
	Log     *slog.Logger
	// Proc is the host's /proc. The installer pod shares the host's PID namespace.
	Proc string

	// afterSubidRead runs while the installer holds shadow's locks, between reading and writing the id files. Tests only.
	afterSubidRead func()
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

	// A failed rollback can leave containerd stopped, so preflight would find no active unit.
	err = in.finishPendingRestart(ctx)
	if err != nil {
		return errors.Join(err, in.reject(ctx, "pending restart: "+err.Error()))
	}

	d, unit, dropIn, kubelet, err := in.preflight(ctx)
	if reject, ok := errors.AsType[rejection](err); ok {
		return in.reject(ctx, "preflight: "+reject.Error())
	}

	if err != nil {
		return err
	}

	in.Log.Info("preflight passed", "distribution", d.name, "unit", unit)

	err = installBundle(in.Bundle, in.HostRoot+Prefix, in.HostRoot+RetiredRoot, nil)
	if err != nil {
		return err
	}

	out, err := in.Host(ctx, Prefix+"/bin/krun", "--version")
	if err != nil || !strings.Contains(out, "+LIBKRUN") {
		return in.reject(ctx, fmt.Sprintf("bundle: krun --version on the host: %v: %s", err, out))
	}

	if kubelet != nil {
		err = in.reserveIDs(*kubelet)
		if reject, ok := errors.AsType[rejection](err); ok {
			return in.reject(ctx, "preflight: "+reject.Error())
		}

		if err != nil {
			return err
		}
	}

	if in.Phase == 2 {
		err = linkShim(in.HostRoot + ShimLink)
		if err != nil {
			return err
		}
	}

	in.removeRetired()
	_ = os.Remove(in.HostRoot + legacyVerifyAfter)

	return in.configure(ctx, in.HostRoot+d.dropInFile(), unit, in.HostRoot+d.socket, dropIn)
}

// removeRetired deletes the retired bundles that no process uses any more, and the staging directory of older
// installers, which can hold a previous bundle. A failure only delays it to the next install.
func (in *Installer) removeRetired() {
	pid, err := removeUnused(in.HostRoot+legacyStaged, in.HostRoot, in.Proc)
	if err != nil || pid != 0 {
		in.Log.Warn("keep "+legacyStaged, "pid", pid, "error", err)
	}

	kept, err := removeRetired(in.HostRoot+RetiredRoot, in.HostRoot, in.Proc)
	if err != nil {
		in.Log.Warn("remove retired bundles", "error", err)
	}

	for name, pid := range kept {
		in.Log.Info("keeping a retired bundle while a process uses it", "bundle", RetiredRoot+"/"+name, "pid", pid)
	}
}

// subidFiles are the subordinate id files, in the order in which shadow's tools lock them.
var subidFiles = []string{"/etc/subuid", "/etc/subgid"}

// reserveIDs adds the sbxr range to /etc/subuid and /etc/subgid, so no other user and no kubelet pod gets these ids.
// It holds shadow's locks from reading the files to writing them, so a concurrent useradd cannot take the same ids.
func (in *Installer) reserveIDs(kubelet subid.Range) error {
	return in.withSubids(func(data [][]byte) ([][]byte, error) {
		r, isNew, err := subid.Choose(string(data[0]), string(data[1]), kubelet)
		if err != nil {
			return nil, rejection{fmt.Errorf("pod VM ids: %w", err)}
		}

		if !isNew {
			return data, nil
		}

		out := make([][]byte, len(data))

		for i, d := range data {
			if len(d) > 0 && !bytes.HasSuffix(d, []byte("\n")) {
				d = append(d, '\n')
			}

			out[i] = append(d, r.String()+"\n"...)
		}

		in.Log.Info("reserving ids for pod VMs", "range", r.String())

		return out, nil
	})
}

// releaseIDs removes the sbxr entries from /etc/subuid and /etc/subgid.
func (in *Installer) releaseIDs() error {
	return in.withSubids(func(data [][]byte) ([][]byte, error) {
		out := make([][]byte, len(data))

		for i, d := range data {
			for line := range strings.Lines(string(d)) {
				if !strings.HasPrefix(line, subid.Name+":") {
					out[i] = append(out[i], line...)
				}
			}
		}

		return out, nil
	})
}

// withSubids reads both subordinate id files under shadow's locks, and writes the files that edit changes before it
// releases the locks. A missing file counts as empty.
func (in *Installer) withSubids(edit func(data [][]byte) ([][]byte, error)) error {
	data := make([][]byte, len(subidFiles))
	unlocks := make([]func(), 0, len(subidFiles))

	// Release in reverse order, after the writes.
	defer func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}()

	for i, name := range subidFiles {
		unlock, err := lockShadow(in.HostRoot + name)
		if err != nil {
			return err
		}

		unlocks = append(unlocks, unlock)

		data[i], err = os.ReadFile(in.HostRoot + name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", name, err)
		}
	}

	if in.afterSubidRead != nil {
		in.afterSubidRead()
	}

	out, err := edit(data)
	if err != nil {
		return err
	}

	for i, name := range subidFiles {
		if !bytes.Equal(out[i], data[i]) {
			if err := writeFile(in.HostRoot+name, out[i]); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		}
	}

	return nil
}

// configure publishes the drop-in, restarts the unit when needed, verifies the handler, and labels the node.
// Any failure after publishing restores the previous drop-in (or removes it), restarts again, and rejects the node.
// The exception is a failed final label patch, an apiError: containerd serves sbxr then, so the drop-in stays and the pod retries.
func (in *Installer) configure(ctx context.Context, path, unit, socket string, dropIn []byte) error {
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read drop-in: %w", err)
	}

	changed := err != nil || !bytes.Equal(previous, dropIn)
	if !changed {
		// The drop-in is already ours, so a rollback removes it.
		previous = nil
	}

	err = in.apply(ctx, path, unit, socket, dropIn, changed)
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
		rollback = in.restart(ctx, unit)
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

// unverified means that containerd did not restart or does not serve the handler.
type unverified struct{ error }

// apply publishes the drop-in when it changed, restarts the unit when a restart is pending, verifies, and labels the node.
func (in *Installer) apply(ctx context.Context, path, unit, socket string, dropIn []byte, changed bool) error {
	if changed {
		err := in.markRestart(unit)
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

		err := in.restart(ctx, unit)
		if err != nil {
			return err
		}
	}

	err := in.waitHandler(ctx, socket)
	if err != nil {
		return err
	}

	err = in.setStatus(ctx, true, new("ok"))
	if err != nil {
		// The node serves sbxr, so a retry only needs to label it.
		return apiError{err}
	}

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

	return in.restart(ctx, unit)
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
// It also returns the pod VM id range of phase 2 when /etc/subuid and /etc/subgid still need it.
func (in *Installer) preflight(ctx context.Context) (distro, string, []byte, *subid.Range, error) {
	d, unit, err := in.detect(ctx)
	if err != nil {
		return distro{}, "", nil, nil, err
	}

	n, err := in.runtimeNode(ctx)
	if err != nil {
		return distro{}, "", nil, nil, err
	}

	config, err := os.ReadFile(in.HostRoot + d.config)
	if err != nil {
		return distro{}, "", nil, nil, rejection{fmt.Errorf("read containerd config: %w", err)}
	}

	_, kvmErr := in.Host(ctx, "test", "-c", "/dev/kvm")
	_, dbusErr := in.Host(ctx, "test", "-S", "/run/dbus/system_bus_socket")
	_, tunErr := in.Host(ctx, "test", "-c", "/dev/net/tun")
	glibc, _ := in.Host(ctx, "getconf", "GNU_LIBC_VERSION")
	kernel, _ := in.Host(ctx, "uname", "-r")
	_, kubeletErr := in.Host(ctx, "getent", "passwd", "kubelet")
	kubeletUIDs := in.hostOutput(ctx, "getsubids", "kubelet")
	kubeletGIDs := in.hostOutput(ctx, "getsubids", "-g", "kubelet")
	subuid, _ := os.ReadFile(in.HostRoot + "/etc/subuid")
	subgid, _ := os.ReadFile(in.HostRoot + "/etc/subgid")

	f := facts{
		kvm:         kvmErr == nil,
		glibc:       glibc,
		runtime:     n.RuntimeVersion,
		config:      string(config),
		systemd:     runcSystemdCgroup(append([]string{string(config)}, in.imported(string(config), d)...)...),
		dbus:        dbusErr == nil,
		kernel:      kernel,
		tun:         tunErr == nil,
		subuid:      string(subuid),
		subgid:      string(subgid),
		kubeletUser: kubeletErr == nil,
		kubeletUIDs: kubeletUIDs,
		kubeletGIDs: kubeletGIDs,
		maxPods:     n.MaxPods,
	}

	dropIn, err := preflight(d, f, in.Phase)
	if err != nil {
		return distro{}, "", nil, nil, rejection{err}
	}

	if in.Phase != 2 {
		return d, unit, dropIn, nil, nil
	}

	kubelet, err := podIDs(f)
	if err != nil {
		return distro{}, "", nil, nil, rejection{err}
	}

	return d, unit, dropIn, &kubelet, nil
}

// hostOutput runs a host command and returns its output, or nothing when it fails.
func (in *Installer) hostOutput(ctx context.Context, args ...string) string {
	out, err := in.Host(ctx, args...)
	if err != nil {
		return ""
	}

	return out
}

// runtimeNode reads the node once the kubelet knows the container runtime version.
// Right after a containerd restart the kubelet reports "containerd://Unknown", which is no reason to reject the node.
func (in *Installer) runtimeNode(ctx context.Context) (Node, error) {
	deadline := time.Now().Add(in.Timeout)

	for {
		n, err := in.API.Node(ctx)
		if err != nil {
			return n, fmt.Errorf("read node: %w", err)
		}

		if !strings.HasSuffix(n.RuntimeVersion, "://Unknown") || time.Now().After(deadline) {
			return n, nil
		}

		select {
		case <-ctx.Done():
			return n, ctx.Err() //nolint:wrapcheck // The installer stops.
		case <-time.After(in.Poll):
		}
	}
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

// restart restarts unit, checks that it is active, and clears the pending restart.
func (in *Installer) restart(ctx context.Context, unit string) error {
	out, err := in.Host(ctx, "systemctl", "restart", unit)
	if err != nil {
		return unverified{fmt.Errorf("restart %s: %w: %s", unit, err, out)}
	}

	out, err = in.Host(ctx, "systemctl", "is-active", unit)
	if err != nil {
		return unverified{fmt.Errorf("%s is not active after the restart: %w: %s", unit, err, strings.TrimSpace(out))}
	}

	err = os.Remove(in.HostRoot + restartPending)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear pending restart: %w", err)
	}

	return nil
}

// criCall bounds one CRI call, so that a containerd that hangs does not block the wait.
const criCall = 5 * time.Second

// waitHandler waits until containerd, asked through the CRI on socket, serves handler sbxr.
// It asks containerd itself rather than the node status: the kubelet posts an unchanged node status only every
// nodeStatusReportFrequency plus a random delay (2.5 to 7.5 minutes by default), and it may still post a handler
// that it cached from the containerd before the restart.
func (in *Installer) waitHandler(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()

	var last error

	for {
		callCtx, callCancel := context.WithTimeout(ctx, criCall)
		handlers, err := in.CRI(callCtx, socket)

		callCancel()

		switch {
		case err != nil:
			last = unverified{err}
		case !slices.Contains(handlers, Handler):
			last = unverified{fmt.Errorf("containerd does not serve runtime handler %s (it serves %s)", Handler, strings.Join(handlers, ", "))}
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
