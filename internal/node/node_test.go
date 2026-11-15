package node

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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/subid"
)

const dropInPath = "/etc/containerd/conf.d/sbxr.toml"

// fakeNode is the API server and the host of one node.
type fakeNode struct {
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
	// stale makes the kubelet post no node status after a restart.
	stale bool
	// nodeReads counts node reads. With failReadsAfter > 0, later reads fail, and with -1, every read fails.
	nodeReads, failReadsAfter int
	// failReady makes the patch that labels the node ready fail.
	failReady bool
	// krunFails makes the bundle self-test on the host fail.
	krunFails bool
}

func (f *fakeNode) Node(context.Context) (Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nodeReads++
	if f.failReadsAfter != 0 && f.nodeReads > f.failReadsAfter {
		return Node{}, errors.New("connection refused")
	}

	heartbeat := time.Now().Add(time.Minute)
	if f.stale {
		heartbeat = time.Unix(1, 0)
	}

	return Node{RuntimeVersion: "containerd://2.3.4", RuntimeHandlers: slices.Clone(f.handlers), Heartbeat: heartbeat, MaxPods: 110}, nil
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
	case "test -S /run/dbus/system_bus_socket":
		return "", nil
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
	case "/opt/sbxr/bin/krun --version":
		if !f.krunFails {
			return "crun version 1.31\n+SYSTEMD +SELINUX +LIBKRUN\n", nil
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
		units:       "dbus.service loaded active running D-Bus\ncontainerd.service loaded active running containerd\n",
		kvm:         true,
		serveSbxr:   true,
	}

	write(t, f.root+"/etc/containerd/config.toml", kindConfig)
	write(t, f.root+"/run/.keep", "")

	return f, &Installer{
		API:      f,
		Host:     f.host,
		HostRoot: f.root,
		Bundle:   newBundle(t, "crun"),
		Phase:    1,
		Poll:     time.Millisecond,
		Timeout:  50 * time.Millisecond,
		Log:      slog.New(slog.DiscardHandler),
	}
}

// newBundle writes a bundle whose crun holds the given content.
func newBundle(t *testing.T, crun string) string {
	t.Helper()

	bundle := t.TempDir()
	write(t, bundle+"/bin/crun", crun)
	write(t, bundle+"/lib/libkrun.so.1.19.8", "libkrun")
	symlink(t, "crun", bundle+"/bin/krun")
	symlink(t, "libkrun.so.1.19.8", bundle+"/lib/libkrun.so.1")
	write(t, bundle+"/SHA256SUMS", sum(crun)+"  ./bin/crun\n"+sum("libkrun")+"  ./lib/libkrun.so.1.19.8\n")

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
	for _, want := range []string{"version = 2", "SystemdCgroup = true", `Root = "/run/sbxr/runc"`} {
		if !strings.Contains(dropIn, want) {
			t.Errorf("drop-in lacks %q:\n%s", want, dropIn)
		}
	}

	if read(t, f.root+"/opt/sbxr/bin/crun") != "crun" || read(t, f.root+"/opt/sbxr/bin/krun") != "crun" {
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
		{"handler missing", "", func(f *fakeNode) { f.serveSbxr = false }, "verify: after 50ms: node does not report runtime handler sbxr"},
		{"upgrade without fresh status", "# older sbxr drop-in\n", func(f *fakeNode) {
			f.handlers = []string{"runc", "sbxr"}
			f.stale = true
		}, "verify: after 50ms: no node status after"},
		{"upgrade loses handler", "# older sbxr drop-in\n", func(f *fakeNode) { f.serveSbxr = false }, "verify: after 50ms: node does not report"},
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

func TestInstallKeepsDropInWhenAPIFailsDuringVerification(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	f.failReadsAfter = 1 // Preflight.

	err := in.Install(t.Context())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error %v, want the API error", err)
	}

	if !exists(f.root+dropInPath) || f.restarts != 1 || f.labels[ReadyLabel] == "true" {
		t.Errorf("drop-in exists %t, restarts %d, labels %v; want the drop-in, 1 restart, no label", exists(f.root+dropInPath), f.restarts, f.labels)
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
	if err != nil || f.restarts != 1 || f.labels[ReadyLabel] != "true" || exists(f.root+verifyAfter) {
		t.Errorf("retry: error %v, restarts %d, labels %v; want ready without a restart", err, f.restarts, f.labels)
	}
}

// TestInstallNeedsStatusAfterRestart checks that a node status from before the last restart never makes the node ready.
func TestInstallNeedsStatusAfterRestart(t *testing.T) {
	t.Parallel()

	t.Run("first install", func(t *testing.T) {
		t.Parallel()

		f, in := newFakeNode(t)
		f.stale = true

		err := in.Install(t.Context())
		if err != nil || f.labels[ReadyLabel] == "true" || !strings.Contains(f.annotations[PreflightAnnotation], "no node status after") {
			t.Errorf("error %v, labels %v, annotations %v; want a rejection", err, f.labels, f.annotations)
		}
	})

	t.Run("retry after an API failure", func(t *testing.T) {
		t.Parallel()

		f, in := newFakeNode(t)

		err := in.Install(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		// An upgrade restarts containerd, then the API server fails during the wait.
		write(t, f.root+dropInPath, "# older sbxr drop-in\n")
		f.failReadsAfter = f.nodeReads + 1

		err = in.Install(t.Context())
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("error %v, want the API error", err)
		}

		// The retry sees only statuses from before the restart.
		f.failReadsAfter = 0
		f.stale = true

		err = in.Install(t.Context())
		if err != nil || f.labels[ReadyLabel] == "true" || !strings.Contains(f.annotations[PreflightAnnotation], "no node status after") {
			t.Errorf("error %v, labels %v, annotations %v; want a rejection", err, f.labels, f.annotations)
		}
	})
}

// laggingAPI reports the node status that the kubelet built from its runtime cache from before the restart, with a new heartbeat.
type laggingAPI struct {
	*fakeNode
}

func (a laggingAPI) Node(ctx context.Context) (Node, error) {
	n, err := a.fakeNode.Node(ctx)
	n.RuntimeHandlers = []string{"runc", "sbxr"}
	n.Heartbeat = time.Now()

	return n, err
}

func TestInstallIgnoresCachedHandlerAfterRestart(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	write(t, f.root+dropInPath, "# older sbxr drop-in\n")
	f.serveSbxr = false
	in.API = laggingAPI{f}

	err := in.Install(t.Context())
	if err != nil || f.labels[ReadyLabel] == "true" || !strings.Contains(f.annotations[PreflightAnnotation], "no node status after") {
		t.Errorf("error %v, labels %v, annotations %v; want a rejection", err, f.labels, f.annotations)
	}
}

// TestInstallGuardsAgainstCachedHandler pins each guard against a node status that still lists the handler of an earlier drop-in.
func TestInstallGuardsAgainstCachedHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(*testing.T, *fakeNode, *Installer)
		restarts int
	}{
		{
			// removeDropIns writes an empty unit when it cannot detect one. Guard: a pending restart is no first load.
			name: "pending restart without unit",
			setup: func(t *testing.T, f *fakeNode, _ *Installer) {
				t.Helper()
				write(t, f.root+restartPending, "\n")
			},
			restarts: 2, // The new drop-in and the rollback.
		},
		{
			// Guard: a first load never moves a persisted cutoff earlier.
			name: "cutoff in the future",
			setup: func(t *testing.T, f *fakeNode, _ *Installer) {
				t.Helper()
				write(t, f.root+verifyAfter, time.Now().Add(time.Hour).Format(time.RFC3339Nano)+"\n")
			},
			restarts: 2,
		},
		{
			// Guard: uninstall keeps the cutoff of its restart.
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
			in.API = laggingAPI{f}

			err := in.Install(t.Context())
			if err != nil || f.restarts != tt.restarts || f.labels[ReadyLabel] == "true" ||
				!strings.Contains(f.annotations[PreflightAnnotation], "no node status after") {
				t.Errorf("error %v, restarts %d, labels %v, annotations %v; want %d restarts and a rejection",
					err, f.restarts, f.labels, f.annotations, tt.restarts)
			}
		})
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
	bundles := []string{in.Bundle, newBundle(t, "crun v2")}

	err := installBundle(bundles[0], dst)
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

				if _, err := os.Stat(dst + "/bin/krun"); err != nil {
					missing.Add(1)
				}
			}
		}()

		for i := range 20 {
			err := installBundle(bundles[i%2], dst)
			if err != nil {
				t.Error(err)
			}
		}

		close(stop)
		<-done

		if missing.Load() > 0 || exists(dst+".new") || exists(dst+".old") {
			t.Errorf("krun missing %d times, files %v", missing.Load(), files(t, f.root+"/opt"))
		}
	})

	t.Run("a bad bundle leaves the installed one", func(t *testing.T) { //nolint:paralleltest // The steps share one install directory.
		bad := newBundle(t, "crun v3")
		write(t, bad+"/bin/crun", "tampered")

		before := read(t, dst+"/bin/crun")

		err := installBundle(bad, dst)
		if err == nil || !strings.Contains(err.Error(), "bin/crun has SHA-256") {
			t.Fatalf("error %v, want a checksum error", err)
		}

		if read(t, dst+"/bin/crun") != before || exists(dst+".new") {
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
	write(t, f.root+RuntimeRoot+"/k8s.io/.keep", "")

	err = in.Uninstall(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if len(f.labels) > 0 || len(f.annotations) > 0 || f.restarts != 2 {
		t.Errorf("labels %v, annotations %v, restarts %d", f.labels, f.annotations, f.restarts)
	}

	for _, p := range []string{dropInPath, "/etc/k0s/containerd.d/sbxr.toml", Prefix, restartPending, "/run/sbxr"} {
		if exists(f.root + p) {
			t.Errorf("%s remains", p)
		}
	}

	if !exists(f.root + verifyAfter) {
		t.Error("the verification cutoff is gone, so a reinstall could trust the removed handler")
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
			name: "container still exists",
			fault: func(t *testing.T, f *fakeNode) {
				t.Helper()
				write(t, f.root+RuntimeRoot+"/k8s.io/0123abcd/state.json", "{}")
				write(t, f.root+RuntimeRoot+"/k8s.io/.cache/x", "")
			},
			fix: func(t *testing.T, f *fakeNode) {
				t.Helper()

				err := os.RemoveAll(f.root + RuntimeRoot + "/k8s.io/0123abcd")
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

			if !exists(f.root+Prefix+"/bin/crun") || exists(f.root+dropInPath) {
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

			in.Bundle = newBundle(t, "crun v2")
			f.krunFails = true

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

func TestInstallPhase2(t *testing.T) {
	t.Parallel()

	f, in := newFakeNode(t)
	in.Phase = 2

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
