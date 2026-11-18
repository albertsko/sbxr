package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/kube/subid"
)

const dropInPath = "/etc/containerd/conf.d/sbxr.toml"

// fakeNode is the API server and the host of one node.
type fakeNode struct {
	// onBundleCheck runs when the installer checks the installed bundle, after preflight.
	onBundleCheck func()

	root string

	mu          sync.Mutex
	labels      map[string]string
	annotations map[string]string
	handlers    []string
	restarts    int
	units       string // Output of systemctl list-units.
	kvm         bool
	// serveSbxr makes containerd serve handler sbxr after a restart when the drop-in exists.
	serveSbxr bool
	// failRestarts makes this many restarts fail, or every restart when negative.
	failRestarts int
	// inactive makes the unit inactive after the next restart.
	inactive bool
	// criDown makes the CRI unreachable.
	criDown bool
	// criSockets records the sockets that the installer asked.
	criSockets []string
	// nodeReads counts node reads. With failReadsAfter > 0, later reads fail, and with -1, every read fails.
	nodeReads, failReadsAfter int
	// failReady makes the patch that labels the node ready fail.
	failReady bool
	// bundleCheckFails makes the bundle self-test on the host fail.
	bundleCheckFails bool
}

func (f *fakeNode) Node(context.Context) (Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nodeReads++
	if f.failReadsAfter != 0 && f.nodeReads > f.failReadsAfter {
		return Node{}, errors.New("connection refused")
	}

	return Node{RuntimeVersion: "containerd://2.3.4", MaxPods: 110}, nil
}

// cri answers the CRI status call with the handlers of the running containerd.
func (f *fakeNode) cri(_ context.Context, socket string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.criSockets = append(f.criSockets, socket)

	if f.criDown {
		return nil, errors.New("CRI unreachable")
	}

	return slices.Clone(f.handlers), nil
}

func (f *fakeNode) Patch(_ context.Context, labels, annotations map[string]*string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failReady && labels[ReadyLabel] != nil {
		return errors.New("injected API failure")
	}

	apply := func(m map[string]string, p map[string]*string) {
		for k, v := range p {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = *v
			}
		}
	}
	apply(f.labels, labels)
	apply(f.annotations, annotations)

	return nil
}

func (f *fakeNode) host(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch strings.Join(args, " ") {
	case "systemctl list-units --type=service --state=active --plain --no-legend --no-pager":
		return f.units, nil
	case "test -c /dev/kvm":
		if f.kvm {
			return "", nil
		}
	case "getconf GNU_LIBC_VERSION":
		return "glibc 2.39\n", nil
	case "uname -r":
		return "6.8.0-1015-azure\n", nil
	case "test -c /dev/net/tun", "getent passwd kubelet":
		return "", nil
	case "getsubids kubelet", "getsubids -g kubelet":
		// getsubids prints the kubelet entry of the file, or fails without one.
		file := map[bool]string{true: "/etc/subgid", false: "/etc/subuid"}[strings.Contains(strings.Join(args, " "), "-g")]
		data, _ := os.ReadFile(f.root + file)

		for _, r := range subid.Parse(string(data)) {
			if r.Name == "kubelet" {
				return fmt.Sprintf("0: kubelet %d %d\n", r.First, r.Count), nil
			}
		}
	case "/opt/sbxr/bin/containerd-shim-sbxr-v1 check":
		if f.onBundleCheck != nil {
			f.onBundleCheck()
		}

		if !f.bundleCheckFails {
			return "libkrun ok, libkrunfw 5.6.4\n", nil
		}
	case "systemctl is-active containerd.service":
		if !f.inactive {
			return "active\n", nil
		}

		f.inactive = false

		return "failed\n", errors.New("exit status 3")
	case "systemctl restart containerd.service":
		f.restarts++

		// A failed restart leaves the unit stopped, and a successful one starts it.
		if f.failRestarts != 0 {
			f.failRestarts--
			f.units = ""

			return "Job for containerd.service failed.", errors.New("exit status 1")
		}

		if !strings.Contains(f.units, "containerd.service") {
			f.units += "containerd.service loaded active running containerd\n"
		}

		f.handlers = []string{"runc"}
		if exists(f.root+dropInPath) && f.serveSbxr {
			f.handlers = append(f.handlers, "sbxr")
		}

		return "", nil
	}

	return "", errors.New("exit status 1")
}

func newFakeNode(t *testing.T) (*fakeNode, *Installer) {
	t.Helper()

	f := &fakeNode{
		root:        t.TempDir(),
		labels:      map[string]string{},
		annotations: map[string]string{},
		handlers:    []string{"runc"},
		units:       "containerd.service loaded active running containerd\n",
		kvm:         true,
		serveSbxr:   true,
	}

	write(t, f.root+"/etc/containerd/config.toml", kindConfig)
	write(t, f.root+"/run/.keep", "")
	write(t, f.root+"/etc/subuid", kubeletIDs)
	write(t, f.root+"/etc/subgid", kubeletIDs)

	return f, &Installer{
		API:      f,
		Host:     f.host,
		CRI:      f.cri,
		HostRoot: f.root,
		Bundle:   newBundle(t, "shim"),
		Poll:     time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Log:      slog.New(slog.DiscardHandler),
		Proc:     t.TempDir(),
	}
}

// kubeletIDs is a valid kubelet entry of /etc/subuid and /etc/subgid for 110 pods.
const kubeletIDs = "kubelet:65536:7208960\n"

// newBundle writes a bundle whose shim holds the given content.
func newBundle(t *testing.T, shim string) string {
	t.Helper()

	bundle := t.TempDir()
	write(t, bundle+"/bin/containerd-shim-sbxr-v1", shim)
	write(t, bundle+"/lib/libkrun.so.1.19.8", "libkrun")
	symlink(t, "libkrun.so.1.19.8", bundle+"/lib/libkrun.so.1")
	write(t, bundle+"/SHA256SUMS", sum(shim)+"  ./bin/containerd-shim-sbxr-v1\n"+sum("libkrun")+"  ./lib/libkrun.so.1.19.8\n")

	return bundle
}

func TestInstall(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	err := in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if f.labels[ReadyLabel] != "true" || f.annotations[PreflightAnnotation] != "ok" || f.restarts != 1 || exists(f.root+restartPending) {
		t.Errorf("labels %v, annotations %v, restarts %d; want ready, ok, 1 restart, nothing pending", f.labels, f.annotations, f.restarts)
	}

	dropIn := read(t, f.root+dropInPath)
	for _, want := range []string{"version = 2", `runtime_type = "io.containerd.sbxr.v1"`} {
		if !strings.Contains(dropIn, want) {
			t.Errorf("drop-in lacks %q:\n%s", want, dropIn)
		}
	}

	if read(t, f.root+"/opt/sbxr/bin/containerd-shim-sbxr-v1") != "shim" {
		t.Error("bundle not installed")
	}

	t.Run("again without restart", func(t *testing.T) {
		err := in.Install(t.Context())
		if err != nil || f.restarts != 1 || f.labels[ReadyLabel] != "true" {
			t.Errorf("error %v, restarts %d, labels %v; want ready and no new restart", err, f.restarts, f.labels)
		}
	})
}

func TestInstallRestartsForAnUnloadedDropIn(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	err := in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// An earlier installer wrote the drop-in and stopped before the restart.
	f.handlers = []string{"runc"}
	write(t, f.root+restartPending, "")

	err = in.Install(t.Context())
	if err != nil || f.restarts != 2 || f.labels[ReadyLabel] != "true" || exists(f.root+restartPending) {
		t.Errorf("error %v, restarts %d, labels %v; want a restart and ready", err, f.restarts, f.labels)
	}
}

func TestInstallRejectsNodeWithoutChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*testing.T, *fakeNode)
		reason string
	}{
		{"no kvm", func(_ *testing.T, f *fakeNode) { f.kvm = false }, "preflight: /dev/kvm is missing"},
		{"no import", func(t *testing.T, f *fakeNode) {
			t.Helper()
			write(t, f.root+"/etc/containerd/config.toml", "version = 2\n")
		}, "preflight: /etc/containerd/config.toml does not import"},
		{"no containerd unit", func(_ *testing.T, f *fakeNode) { f.units = "crio.service loaded active running\n" }, "preflight: no supported containerd unit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, in := newFakeNode(t)
			f.labels[ReadyLabel] = "true"
			tt.change(t, f)
			config := read(t, f.root+"/etc/containerd/config.toml")

			err := in.Install(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			if _, ok := f.labels[ReadyLabel]; ok || !strings.HasPrefix(f.annotations[PreflightAnnotation], tt.reason) {
				t.Errorf("labels %v, annotations %v; want no label and reason %q", f.labels, f.annotations, tt.reason)
			}

			if f.restarts != 0 || read(t, f.root+"/etc/containerd/config.toml") != config ||
				exists(f.root+"/etc/containerd/conf.d") || exists(f.root+Prefix) || exists(f.root+stateDir) {
				t.Errorf("node changed: %d restarts, files %v", f.restarts, files(t, f.root))
			}
		})
	}
}

// TestInstallRollsBack injects a failure after the drop-in is published and checks that the previous drop-in comes back.
func TestInstallRollsBack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		previous string // Drop-in before the install, or empty for none.
		fault    func(*fakeNode)
		reason   string
	}{
		{"restart fails", "", func(f *fakeNode) { f.failRestarts = 1 }, "verify: restart containerd.service"},
		{"unit inactive", "", func(f *fakeNode) { f.inactive = true }, "verify: containerd.service is not active after the restart"},
		{"handler missing", "", func(f *fakeNode) { f.serveSbxr = false }, "verify: after 50ms: containerd does not serve runtime handler sbxr"},
		{"CRI unreachable", "# older sbxr drop-in\n", func(f *fakeNode) { f.criDown = true }, "verify: after 50ms: CRI unreachable"},
		{"upgrade loses handler", "# older sbxr drop-in\n", func(f *fakeNode) { f.serveSbxr = false }, "verify: after 50ms: containerd does not serve"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, in := newFakeNode(t)
			if tt.previous != "" {
				write(t, f.root+dropInPath, tt.previous)
			}

			tt.fault(f)

			err := in.Install(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			if _, ok := f.labels[ReadyLabel]; ok || !strings.HasPrefix(f.annotations[PreflightAnnotation], tt.reason) {
				t.Errorf("labels %v, annotation %q; want no label and reason %q", f.labels, f.annotations[PreflightAnnotation], tt.reason)
			}

			if got := readOptional(f.root + dropInPath); got != tt.previous || f.restarts != 2 || exists(f.root+restartPending) {
				t.Errorf("drop-in %q after %d restarts, want %q after 2 and nothing pending", got, f.restarts, tt.previous)
			}
		})
	}
}

func TestInstallKeepsDropInWhenFinalPatchFails(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	f.failReady = true

	err := in.Install(t.Context())
	if err == nil || !strings.Contains(err.Error(), "injected API failure") {
		t.Fatalf("error %v, want the patch error", err)
	}

	if !exists(f.root+dropInPath) || f.restarts != 1 || f.labels[ReadyLabel] == "true" {
		t.Errorf("drop-in exists %t, restarts %d, labels %v; want the drop-in, 1 restart, no label", exists(f.root+dropInPath), f.restarts, f.labels)
	}

	f.failReady = false

	err = in.Install(t.Context())
	if err != nil || f.restarts != 1 || f.labels[ReadyLabel] != "true" {
		t.Errorf("retry: error %v, restarts %d, labels %v; want ready without a restart", err, f.restarts, f.labels)
	}
}

// TestInstallAsksContainerd pins that only the restarted containerd decides. A kubelet that still lists the handler
// of an earlier drop-in in the node status, from its cache, cannot make the node ready.
func TestInstallAsksContainerd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(*testing.T, *fakeNode, *Installer)
		restarts int
	}{
		{
			name: "upgrade",
			setup: func(t *testing.T, f *fakeNode, _ *Installer) {
				t.Helper()
				write(t, f.root+dropInPath, "# older sbxr drop-in\n")
				f.handlers = []string{"runc", "sbxr"} // The containerd before the restart served it.
			},
			restarts: 2, // The new drop-in and the rollback.
		},
		{
			// removeDropIns writes an empty unit when it cannot detect one.
			name: "pending restart without unit",
			setup: func(t *testing.T, f *fakeNode, _ *Installer) {
				t.Helper()
				write(t, f.root+restartPending, "\n")
			},
			restarts: 2,
		},
		{
			name: "reinstall after uninstall",
			setup: func(t *testing.T, _ *fakeNode, in *Installer) {
				t.Helper()

				err := errors.Join(in.Install(t.Context()), in.Uninstall(t.Context()))
				if err != nil {
					t.Fatal(err)
				}
			},
			restarts: 4, // Install and uninstall, then the new drop-in and the rollback.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, in := newFakeNode(t)
			tt.setup(t, f, in)
			f.serveSbxr = false

			err := in.Install(t.Context())
			if err != nil || f.restarts != tt.restarts || f.labels[ReadyLabel] == "true" ||
				!strings.Contains(f.annotations[PreflightAnnotation], "containerd does not serve runtime handler sbxr") {
				t.Errorf("error %v, restarts %d, labels %v, annotations %v; want %d restarts and a rejection",
					err, f.restarts, f.labels, f.annotations, tt.restarts)
			}
		})
	}
}

// TestInstallVerifiesInSeconds checks that the installer asks the distribution's containerd socket, and that a
// verification cutoff of an older installer neither delays the install nor stays.
func TestInstallVerifiesInSeconds(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	in.Timeout = time.Hour
	write(t, f.root+legacyVerifyAfter, time.Now().Add(time.Hour).Format(time.RFC3339Nano)+"\n")

	start := time.Now()

	err := in.Install(t.Context())
	if err != nil || f.labels[ReadyLabel] != "true" || time.Since(start) > 10*time.Second {
		t.Fatalf("install: %v after %s, labels %v", err, time.Since(start), f.labels)
	}

	if exists(f.root+legacyVerifyAfter) || !slices.Contains(f.criSockets, f.root+"/run/containerd/containerd.sock") {
		t.Errorf("verify-after left %v, CRI sockets %v", exists(f.root+legacyVerifyAfter), f.criSockets)
	}
}

func TestInstallFinishesPendingRestartWithoutAPI(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	write(t, f.root+restartPending, "containerd.service\n")
	f.units = ""
	f.failReadsAfter = -1

	err := in.Install(t.Context())
	if err == nil || f.restarts != 1 || exists(f.root+restartPending) || f.units == "" {
		t.Errorf("error %v, restarts %d, units %q; want the restart and then the API error", err, f.restarts, f.units)
	}
}

func TestInstallFinishesRestartAfterFailedRollback(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	err := in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Verification fails, and the rollback restart fails too, which leaves containerd stopped.
	f.handlers = []string{"runc"}
	f.failRestarts = 1

	err = in.Install(t.Context())
	if err == nil || f.units != "" {
		t.Fatalf("error %v, units %q; want a failed rollback and a stopped unit", err, f.units)
	}

	restarts := f.restarts

	err = in.Install(t.Context())
	if err != nil || f.restarts != restarts+2 || f.labels[ReadyLabel] != "true" {
		t.Errorf("error %v, restarts %d after %d, labels %v; want the pending restart, a new install, and ready", err, f.restarts, restarts, f.labels)
	}
}

func TestInstallFailsWhenRollbackFails(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	err := in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	f.handlers = []string{"runc"}
	f.failRestarts = -1

	err = in.Install(t.Context())
	if err == nil || !strings.Contains(err.Error(), "roll back: restart containerd.service") {
		t.Fatalf("error %v, want a rollback error", err)
	}

	if _, ok := f.labels[ReadyLabel]; ok || !strings.HasPrefix(f.annotations[PreflightAnnotation], "verify: ") {
		t.Errorf("labels %v, annotations %v; want no label and the reason", f.labels, f.annotations)
	}
}

func TestInstallBundle(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	dst := f.root + Prefix
	bundles := []string{in.Bundle, newBundle(t, "shim v2")}

	err := installBundle(bundles[0], dst, dst+".old", nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the runtime path never vanishes", func(t *testing.T) { //nolint:paralleltest // The steps share one install directory.
		var missing atomic.Int64

		stop := make(chan struct{})
		done := make(chan struct{})

		go func() {
			defer close(done)

			for {
				select {
				case <-stop:
					return
				default:
				}

				if _, err := os.Stat(dst + "/bin/containerd-shim-sbxr-v1"); err != nil {
					missing.Add(1)
				}
			}
		}()

		for i := range 20 {
			err := installBundle(bundles[i%2], dst, dst+".old", nil)
			if err != nil {
				t.Error(err)
			}
		}

		close(stop)
		<-done

		// No process uses the previous bundles, so they all go.
		kept, err := removeRetired(dst+".old", f.root, t.TempDir())
		if err != nil || len(kept) > 0 {
			t.Errorf("removeRetired: kept %v, %v", kept, err)
		}

		if entries, _ := os.ReadDir(dst + ".old"); missing.Load() > 0 || exists(dst+".new") || len(entries) > 0 {
			t.Errorf("shim missing %d times, files %v", missing.Load(), files(t, f.root+"/opt"))
		}
	})

	t.Run("a bad bundle leaves the installed one", func(t *testing.T) { //nolint:paralleltest // The steps share one install directory.
		bad := newBundle(t, "shim v3")
		write(t, bad+"/bin/containerd-shim-sbxr-v1", "tampered")

		before := read(t, dst+"/bin/containerd-shim-sbxr-v1")

		err := installBundle(bad, dst, dst+".old", nil)
		if err == nil || !strings.Contains(err.Error(), "bin/containerd-shim-sbxr-v1 has SHA-256") {
			t.Fatalf("error %v, want a checksum error", err)
		}

		if entries, _ := os.ReadDir(dst + ".old"); read(t, dst+"/bin/containerd-shim-sbxr-v1") != before || len(entries) > 0 {
			t.Errorf("bundle changed: %v", files(t, f.root+"/opt"))
		}
	})
}

func TestUninstall(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	err := in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A drop-in of another distribution goes too.
	write(t, f.root+"/etc/k0s/containerd.d/sbxr.toml", "stale")
	write(t, f.root+RunRoot+"/.keep", "")
	write(t, f.root+legacyVerifyAfter, "2026-11-15T00:00:00Z\n") // From an older installer.

	err = in.Uninstall(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(f.labels) > 0 || len(f.annotations) > 0 || f.restarts != 2 {
		t.Errorf("labels %v, annotations %v, restarts %d", f.labels, f.annotations, f.restarts)
	}

	for _, p := range []string{dropInPath, "/etc/k0s/containerd.d/sbxr.toml", Prefix, restartPending, legacyVerifyAfter, "/run/sbxr"} {
		if exists(f.root + p) {
			t.Errorf("%s remains", p)
		}
	}

	if !exists(f.root + lockFile) {
		t.Error("the lock file is gone, so a waiting installer and a new one could lock different inodes")
	}

	t.Run("again without restart", func(t *testing.T) {
		err := in.Uninstall(t.Context())
		if err != nil || f.restarts != 2 {
			t.Errorf("error %v, restarts %d; want no new restart", err, f.restarts)
		}
	})
}

func TestUninstallKeepsBundleUntilSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fault  func(*testing.T, *fakeNode)
		fix    func(*testing.T, *fakeNode)
		reason string
	}{
		{
			name:   "no unit to restart",
			fault:  func(_ *testing.T, f *fakeNode) { f.units = "" },
			fix:    func(_ *testing.T, f *fakeNode) { f.units = "containerd.service loaded active running\n" },
			reason: "uninstall: drop-in removed, but containerd cannot restart",
		},
		{
			name:   "restart fails",
			fault:  func(_ *testing.T, f *fakeNode) { f.failRestarts = 1 },
			fix:    func(*testing.T, *fakeNode) {},
			reason: "uninstall: restart containerd.service",
		},
		{
			name: "pod still exists",
			fault: func(t *testing.T, f *fakeNode) {
				t.Helper()
				write(t, f.root+PodsRoot+"/0123abcd/vm.pid", "42")
			},
			fix: func(t *testing.T, f *fakeNode) {
				t.Helper()

				err := os.RemoveAll(f.root + PodsRoot + "/0123abcd")
				if err != nil {
					t.Fatal(err)
				}
			},
			reason: "uninstall: 1 sbxr containers still exist (0123abcd)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, in := newFakeNode(t)

			err := in.Install(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			tt.fault(t, f)

			err = in.Uninstall(t.Context())
			if err == nil || !strings.HasPrefix(f.annotations[PreflightAnnotation], tt.reason) {
				t.Fatalf("error %v, annotation %q; want an error and reason %q", err, f.annotations[PreflightAnnotation], tt.reason)
			}

			if !exists(f.root+Prefix+"/bin/containerd-shim-sbxr-v1") || exists(f.root+dropInPath) {
				t.Errorf("files %v; want the bundle and no drop-in", files(t, f.root))
			}

			tt.fix(t, f)

			err = in.Uninstall(t.Context())
			if err != nil || exists(f.root+Prefix) || len(f.annotations) > 0 || f.restarts < 2 {
				t.Errorf("retry: error %v, annotations %v, restarts %d, files %v", err, f.annotations, f.restarts, files(t, f.root))
			}
		})
	}
}

// TestUninstallRestartsAfterFailedReinstall checks that containerd restarts between a failed uninstall and the removal of the bundle,
// even when a reinstall runs in between and replaces the bundle.
func TestUninstallRestartsAfterFailedReinstall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		recovers bool // The reinstall restarts containerd.
	}{
		{"reinstall cannot restart", false},
		{"reinstall restarts", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, in := newFakeNode(t)

			err := in.Install(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			f.failRestarts = 1

			err = in.Uninstall(t.Context())
			if err == nil {
				t.Fatal("uninstall succeeded without a restart")
			}

			failed := f.restarts

			in.Bundle = newBundle(t, "shim v2")
			f.bundleCheckFails = true

			if !tt.recovers {
				f.failRestarts = 1
			}

			_ = in.Install(t.Context())

			err = in.Uninstall(t.Context())

			// Either the reinstall restarts, or its restart fails and the uninstall retry restarts.
			want := failed + 1
			if !tt.recovers {
				want = failed + 2
			}

			if err != nil || f.restarts != want || exists(f.root+Prefix) {
				t.Errorf("error %v, restarts %d, want %d, bundle exists %t", err, f.restarts, want, exists(f.root+Prefix))
			}
		})
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path, []byte(data), 0o755)
	}

	if err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()

	err := os.Symlink(target, path)
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func readOptional(path string) string {
	data, _ := os.ReadFile(path)

	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

func files(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, _ error) error {
		out = append(out, strings.TrimPrefix(path, root))

		return nil
	})

	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))

	return hex.EncodeToString(h[:])
}

func TestInstallReservesIDs(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	write(t, f.root+"/etc/subuid", "")
	write(t, f.root+"/etc/subgid", "")

	// Without a kubelet range, kubelet pods with user namespaces could get any id.
	err := in.Install(t.Context())
	if err != nil || !strings.Contains(f.annotations[PreflightAnnotation], "kubelet id range: getsubids kubelet") {
		t.Fatalf("install without a kubelet range: %v, annotations %v", err, f.annotations)
	}

	write(t, f.root+"/etc/subuid", "kubelet:65536:7208960\n")
	write(t, f.root+"/etc/subgid", "kubelet:65536:7208960")

	// shadow's tools keep the file mode, and a lock of a process that is gone is stale.
	if err := os.Chmod(f.root+"/etc/subgid", 0o600); err != nil {
		t.Fatal(err)
	}

	write(t, f.root+"/etc/subuid.lock", "999999999")

	err = in.Install(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"/etc/subuid", "/etc/subgid"} {
		if got := read(t, f.root+name); got != "kubelet:65536:7208960\nsbxr:7274496:33554432\n" {
			t.Errorf("%s = %q, want the sbxr range after the kubelet range", name, got)
		}
	}

	if st, err := os.Stat(f.root + "/etc/subgid"); err != nil || st.Mode().Perm() != 0o600 || exists(f.root+"/etc/subuid.lock") {
		t.Errorf("subgid mode %v, %v, lock left %v; want the mode kept and the lock gone", st.Mode(), err, exists(f.root+"/etc/subuid.lock"))
	}

	dropIn := read(t, f.root+dropInPath)
	if f.labels[ReadyLabel] != "true" || !strings.Contains(dropIn, `runtime_type = "io.containerd.sbxr.v1"`) {
		t.Errorf("labels %v, drop-in:\n%s", f.labels, dropIn)
	}

	if target, err := os.Readlink(f.root + ShimLink); err != nil || target != "/opt/sbxr/bin/containerd-shim-sbxr-v1" {
		t.Errorf("shim link: %q, %v", target, err)
	}

	// A pod keeps the bundle in place.
	write(t, f.root+PodsRoot+"/pod1/vm.pid", "42")

	err = in.Uninstall(t.Context())
	if err == nil || !strings.Contains(f.annotations[PreflightAnnotation], "1 sbxr containers still exist (pod1)") || !exists(f.root+Prefix) {
		t.Errorf("error %v, annotations %v; want the bundle kept while pod1 exists", err, f.annotations)
	}

	err = os.RemoveAll(f.root + PodsRoot)
	if err == nil {
		err = in.Uninstall(t.Context())
	}

	if _, lerr := os.Lstat(f.root + ShimLink); err != nil || lerr == nil {
		t.Errorf("uninstall: %v; shim link: %v, want removed", err, lerr)
	}

	if got := read(t, f.root+"/etc/subuid"); got != "kubelet:65536:7208960\n" {
		t.Errorf("subuid after uninstall = %q", got)
	}
}

// unknownRuntimeAPI reports the runtime version as unknown for the first reads, as the kubelet does after a containerd restart.
type unknownRuntimeAPI struct {
	*fakeNode
}

func (a unknownRuntimeAPI) Node(ctx context.Context) (Node, error) {
	n, err := a.fakeNode.Node(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.nodeReads <= 2 {
		n.RuntimeVersion = "containerd://Unknown"
	}

	return n, err
}

func TestInstallWaitsForTheRuntimeVersion(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	in.API = unknownRuntimeAPI{f}

	err := in.Install(t.Context())
	if err != nil || f.labels[ReadyLabel] != "true" {
		t.Errorf("error %v, labels %v, annotations %v; want the node ready", err, f.labels, f.annotations)
	}
}

// TestReserveIDsChoosesUnderTheLock simulates useradd, which takes shadow's lock, around the installer's id reservation.
// An entry added after preflight still counts, and a useradd during the reservation waits for it and sees the sbxr range.
func TestReserveIDsChoosesUnderTheLock(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	// After preflight chose 7274496, a user takes the ids right there.
	f.onBundleCheck = func() {
		data := read(t, f.root+"/etc/subuid")
		write(t, f.root+"/etc/subuid", data+"alice:7274496:65536\n")
	}

	// During the reservation, another useradd waits for the lock and then reads the file.
	seen := make(chan string, 1)
	locked := make(chan struct{})

	in.afterSubidRead = func() {
		go func() {
			close(locked)

			unlock, err := lockShadow(f.root + "/etc/subuid")
			if err != nil {
				seen <- err.Error()

				return
			}
			defer unlock()

			data, _ := os.ReadFile(f.root + "/etc/subuid")
			seen <- string(data)
		}()

		<-locked
		time.Sleep(50 * time.Millisecond)

		select {
		case got := <-seen:
			t.Errorf("useradd got the lock during the reservation and read %q", got)
		default:
		}
	}

	err := in.Install(t.Context())
	if err != nil || f.labels[ReadyLabel] != "true" {
		t.Fatalf("install: %v, labels %v, annotations %v", err, f.labels, f.annotations)
	}

	if got := read(t, f.root+"/etc/subuid"); got != "kubelet:65536:7208960\nalice:7274496:65536\nsbxr:7340032:33554432\n" {
		t.Errorf("subuid = %q, want the sbxr range after alice's", got)
	}

	if got := <-seen; !strings.Contains(got, "sbxr:7340032:33554432") {
		t.Errorf("the waiting useradd read %q, want the sbxr range", got)
	}
}

// fakeProc builds a /proc where process pid has the given open file and mapped file, as host paths.
func fakeProc(t *testing.T, proc string, pid int, fd, mapped string) {
	t.Helper()

	dir := filepath.Join(proc, strconv.Itoa(pid))

	if fd != "" {
		if err := os.MkdirAll(dir+"/fd", 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(fd, dir+"/fd/3"); err != nil {
			t.Fatal(err)
		}
	}

	if mapped != "" {
		write(t, dir+"/maps", "7f0000000000-7f0000001000 r-xp 00000000 08:01 1234   "+mapped+"\n")
	}
}

func TestRemoveRetired(t *testing.T) {
	t.Parallel()

	root, proc := t.TempDir(), t.TempDir()
	retired := root + RetiredRoot

	for _, name := range []string{"a", "b", "c"} {
		write(t, retired+"/"+name+"/guest/usr/bin/crun", "crun")
	}

	fakeProc(t, proc, 100, "/opt/sbxr.old/a/guest", "")                                      // A VM's guest root.
	fakeProc(t, proc, 101, "", root+"/opt/sbxr.old/b/lib/libkrun.so.1")                      // A VM's library, seen below the host root.
	fakeProc(t, proc, 102, "/opt/sbxr.old/cc/guest", "/opt/sbxr/lib/libkrun.so.1 (deleted)") // Neither is c.

	kept, err := removeRetired(retired, root, proc)
	if err != nil || len(kept) != 2 || kept["a"] != 100 || kept["b"] != 101 {
		t.Errorf("kept %v, %v; want a and b", kept, err)
	}

	if exists(retired+"/c") || !exists(retired+"/a/guest/usr/bin/crun") {
		t.Errorf("files %v; want c gone and a kept", files(t, retired))
	}
}

// TestUpgradeKeepsTheGuestRoot installs a new bundle while a VM runs from the old one.
// The old bundle stays until the VM is gone, and uninstall waits for it too.
func TestUpgradeKeepsTheGuestRoot(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)

	if err := in.Install(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The VM holds the guest root of the installed bundle. After the rename its fd shows the retired path,
	// which the fake /proc names with the retired directory's real name below.
	in.Bundle = newBundle(t, "shim v2")

	if err := in.Install(t.Context()); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(f.root + RetiredRoot)
	if len(entries) != 0 {
		t.Fatalf("retired %v without a VM, want them removed", entries)
	}

	in.Bundle = newBundle(t, "shim v3")
	old := read(t, f.root+Prefix+"/bin/containerd-shim-sbxr-v1")

	f.onBundleCheck = func() {
		// installBundle retired the previous bundle just before the installer checks the new one.
		entries, _ := os.ReadDir(f.root + RetiredRoot)
		for _, e := range entries {
			fakeProc(t, in.Proc, 4242, RetiredRoot+"/"+e.Name()+"/guest", "")
		}
	}

	if err := in.Install(t.Context()); err != nil {
		t.Fatal(err)
	}

	entries, _ = os.ReadDir(f.root + RetiredRoot)
	if len(entries) != 1 || read(t, f.root+RetiredRoot+"/"+entries[0].Name()+"/bin/containerd-shim-sbxr-v1") != old {
		t.Fatalf("retired %v, want the previous bundle kept for the VM", entries)
	}

	err := in.Uninstall(t.Context())
	if err == nil || !strings.Contains(f.annotations[PreflightAnnotation], "process 4242 still uses the retired bundle") {
		t.Errorf("uninstall: %v, annotations %v; want it to wait for the VM", err, f.annotations)
	}

	// The VM stopped.
	if err := os.RemoveAll(filepath.Join(in.Proc, "4242")); err != nil {
		t.Fatal(err)
	}

	if err := in.Uninstall(t.Context()); err != nil || exists(f.root+RetiredRoot) || exists(f.root+Prefix) {
		t.Errorf("uninstall: %v, files %v", err, files(t, f.root+"/opt"))
	}
}

// TestInstallBundleFailsAfterExchange stops an install right after the exchange, then retries. The previous bundle is
// already in the retired directory, so neither the retry nor the cleanup deletes it while a VM holds its guest root.
func TestInstallBundleFailsAfterExchange(t *testing.T) {
	t.Parallel()

	root, proc := t.TempDir(), t.TempDir()
	dst, retired := root+Prefix, root+RetiredRoot
	v1, v2 := newBundle(t, "shim v1"), newBundle(t, "shim v2")

	if err := installBundle(v1, dst, retired, nil); err != nil {
		t.Fatal(err)
	}

	crash := errors.New("crash")

	err := installBundle(v2, dst, retired, func() error {
		// The VM of a running pod holds the guest root of the previous bundle.
		entries, _ := os.ReadDir(retired)
		for _, e := range entries {
			fakeProc(t, proc, 4242, RetiredRoot+"/"+e.Name()+"/guest", "")
		}

		return crash
	})
	if !errors.Is(err, crash) {
		t.Fatalf("install: %v, want the crash", err)
	}

	// The retry finds v2 installed, and the cleanup keeps the bundle that the VM uses.
	if err := installBundle(v2, dst, retired, nil); err != nil {
		t.Fatal(err)
	}

	kept, err := removeRetired(retired, root, proc)
	if err != nil || len(kept) != 1 {
		t.Fatalf("kept %v, %v; want the previous bundle", kept, err)
	}

	for name := range kept {
		if got := read(t, retired+"/"+name+"/bin/containerd-shim-sbxr-v1"); got != "shim v1" {
			t.Errorf("retired shim %q, want shim v1", got)
		}
	}
}

func TestRemoveUnusedLegacyStaging(t *testing.T) {
	t.Parallel()

	root, proc := t.TempDir(), t.TempDir()
	write(t, root+legacyStaged+"/guest/usr/bin/crun", "crun")
	fakeProc(t, proc, 7, legacyStaged+"/guest", "")

	if pid, err := removeUnused(root+legacyStaged, root, proc); err != nil || pid != 7 || !exists(root+legacyStaged+"/guest/usr/bin/crun") {
		t.Errorf("removeUnused = %d, %v; want it kept for pid 7", pid, err)
	}

	if err := os.RemoveAll(proc + "/7"); err != nil {
		t.Fatal(err)
	}

	if pid, err := removeUnused(root+legacyStaged, root, proc); err != nil || pid != 0 || exists(root+legacyStaged) {
		t.Errorf("removeUnused = %d, %v; want it removed", pid, err)
	}
}

func TestDirUsersErrors(t *testing.T) {
	t.Parallel()

	root, proc := t.TempDir(), t.TempDir()
	dir := root + RetiredRoot + "/a"

	if _, err := dirUsers(proc+"/missing", root, []string{dir}); err == nil {
		t.Error("dirUsers without /proc: no error")
	}

	// A process that has exited leaves an empty directory: it uses nothing.
	if err := os.MkdirAll(proc+"/10", 0o755); err != nil {
		t.Fatal(err)
	}

	users, err := dirUsers(proc, root, []string{dir})
	if err != nil || len(users) != 0 {
		t.Errorf("users %v, %v; want none", users, err)
	}

	// A process whose files cannot be listed may use anything.
	write(t, proc+"/11/fd", "not a directory")

	users, err = dirUsers(proc, root, []string{dir})
	if err != nil || users[dir] != 11 {
		t.Errorf("users %v, %v; want pid 11 for the unreadable process", users, err)
	}
}
