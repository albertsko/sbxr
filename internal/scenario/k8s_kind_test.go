//go:build kind

package scenario_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/albertsko/sbxr/internal/scenario"
)

// agent-sandbox release manifest and its SHA-256.
const (
	agentSandboxURL    = "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.11/sandbox.yaml"
	agentSandboxSHA256 = "4c54dc1284e9d34e9a00b88de2cb0f098f2dbce4ade814616fd49d44cfd5d1c2"
)

// TestKind runs the scenarios for the k8s-p1 or k8s-p2 capabilities and the PRD Kubernetes criteria in a kind cluster.
// scripts/kind-up.sh creates or reuses the cluster and installs the phase in SBXR_PHASE (default 2). The last subtests uninstall it.
// With SBXR_UPGRADE_FROM=1, the cluster runs phase 1 first, so the phase 1 criteria run after the node upgrade.
func TestKind(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("kind with KVM needs Linux")
	}

	scenario.RequireHypervisor(t)

	cluster := os.Getenv("SBXR_KIND_CLUSTER")
	if cluster == "" {
		cluster = "sbxr"
	}

	setup := exec.CommandContext(t.Context(), "../../scripts/kind-up.sh")
	setup.Stdout, setup.Stderr = os.Stderr, os.Stderr

	err := setup.Run()
	if err != nil {
		t.Fatalf("kind setup: %v", err)
	}

	phase := 2
	if os.Getenv("SBXR_PHASE") == "1" {
		phase = 1
	}

	images := kindImages(t, cluster)
	k := &kind{t: t, K8s: &scenario.K8s{Phase: phase, Context: "kind-" + cluster, Node: cluster + "-control-plane", Namespace: "sbxr-test", Images: images}}
	k.image = k.run("", "-n", "sbxr-system", "get", "ds", "sbxr-node", "-o", "jsonpath={.spec.template.spec.containers[0].image}")

	tests := map[string]func(*testing.T, *kind){
		"node is ready and the RuntimeClass exists":  testInstalled,
		"pods select ready nodes":                    testScheduling,
		"probes, Services, pod-to-pod, port-forward": testNetwork,
		"DNS from Go, glibc, and musl":               testDNS,
		"admission policies":                         testAdmission,
		"deletion stops the VM within the grace":     testDeletion,
		"agent-sandbox Sandbox":                      testAgentSandbox,
		"overhead covers the VM":                     testOverhead,
	}

	if phase == 1 {
		tests["one VM per app container"] = testVMPerContainer
	} else {
		maps.Copy(tests, phase2Tests)
	}

	t.Run("phase "+strconv.Itoa(phase), func(t *testing.T) {
		t.Run("scenarios", func(t *testing.T) {
			t.Parallel()

			scenario.RunAll(t, func(*testing.T) scenario.Target {
				return &scenario.K8s{Phase: phase, Context: k.Context, Node: k.Node, Namespace: k.Namespace, Images: images}
			})
		})

		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				test(t, k.with(t))
			})
		}
	})

	// These run after every other subtest, one at a time.
	t.Run("uninstall leaves nothing", func(t *testing.T) { testUninstall(t, k.with(t)) })                 //nolint:paralleltest // See above.
	t.Run("preflight failure changes nothing", func(t *testing.T) { testPreflightFailure(t, k.with(t)) }) //nolint:paralleltest // See above.
}

// kind wraps the target with test helpers that fail t.
type kind struct {
	*scenario.K8s

	t     *testing.T
	image string // The installer image.
}

func (k *kind) with(t *testing.T) *kind {
	t.Helper()

	return &kind{K8s: k.K8s, t: t, image: k.image}
}

// run runs kubectl and returns its trimmed output.
func (k *kind) run(stdin string, args ...string) string {
	k.t.Helper()

	out, err := k.Kubectl(k.t.Context(), []byte(stdin), args...)
	if err != nil {
		k.t.Fatal(err)
	}

	return strings.TrimSpace(out)
}

func (k *kind) apply(yaml string) { k.t.Helper(); k.run(yaml, "-n", k.Namespace, "apply", "-f", "-") }

func (k *kind) node(script string) string {
	k.t.Helper()

	out, err := exec.CommandContext(k.t.Context(), "docker", "exec", k.Node, "sh", "-c", script).CombinedOutput()
	if err != nil {
		k.t.Fatalf("on node: %s: %v: %s", script, err, out)
	}

	return strings.TrimSpace(string(out))
}

// pod returns a pod manifest with runtimeClassName sbxr and the given container lines, indented by 6 spaces.
func pod(name, extraSpec, container string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  labels: {app: %s}
spec:
  runtimeClassName: sbxr
  terminationGracePeriodSeconds: 1
%s  containers:
    - name: c
      imagePullPolicy: IfNotPresent
%s`, name, name, extraSpec, container)
}

func testInstalled(t *testing.T, k *kind) {
	t.Helper()

	labels := k.run("", "get", "node", k.Node, "-o", `jsonpath={.metadata.labels.sbxr\.dev/ready} {.metadata.annotations.sbxr\.dev/preflight}`)
	if labels != "true ok" {
		t.Errorf("node label and annotation %q, want \"true ok\"", labels)
	}

	rc := k.run("", "get", "runtimeclass", "sbxr", "-o", `jsonpath={.handler} {.metadata.annotations.sbxr\.dev/phase} {.scheduling.nodeSelector}`)
	if rc != "sbxr "+strconv.Itoa(k.Phase)+` {"sbxr.dev/ready":"true"}` {
		t.Errorf("RuntimeClass %q", rc)
	}
}

func testScheduling(t *testing.T, k *kind) {
	t.Helper()

	out := k.run(pod("sched", "", "      image: sbxr-test/busybox:1\n"), "-n", k.Namespace, "create", "--dry-run=server", "-f", "-",
		"-o", `jsonpath={.spec.nodeSelector} {.spec.overhead.memory}`)
	overhead := k.run("", "get", "runtimeclass", "sbxr", "-o", "jsonpath={.overhead.podFixed.memory}")

	if want := `{"sbxr.dev/ready":"true"} ` + overhead; out != want {
		t.Errorf("pod nodeSelector and overhead %q, want %q", out, want)
	}
}

func testVMPerContainer(t *testing.T, k *kind) {
	t.Helper()

	k.apply(pod("vm", "", "      image: sbxr-test/busybox:1\n      command: [sleep, \"600\"]\n"))
	k.run("", "-n", k.Namespace, "wait", "pod/vm", "--for=condition=Ready", "--timeout=60s")

	id := strings.TrimPrefix(k.run("", "-n", k.Namespace, "get", "pod", "vm", "-o", "jsonpath={.status.containerStatuses[0].containerID}"), "containerd://")
	comms := k.node(fmt.Sprintf(`c=$(crictl inspect -o go-template --template '{{.info.pid}} {{.info.sandboxID}}' %s)
cat /proc/${c%% *}/comm
cat /proc/$(crictl inspectp -o go-template --template '{{.info.pid}}' ${c#* })/comm`, id))

	app, sandbox, _ := strings.Cut(comms, "\n")
	if !strings.HasPrefix(app, "VM:") || sandbox != "pause" {
		t.Errorf("app container process %q, sandbox process %q; want a VM and a normal pause process", app, sandbox)
	}

	// The uninstall guard looks for this state.
	if state := k.node("ls -d /run/sbxr/runc/k8s.io/" + id + " 2>&1 || true"); state != "/run/sbxr/runc/k8s.io/"+id {
		t.Errorf("crun state of the running container: %q", state)
	}

	k.run("", "-n", k.Namespace, "delete", "pod", "vm", "--wait")
}

func testNetwork(t *testing.T, k *kind) {
	t.Helper()

	server := pod("web", "", `      image: sbxr-test/busybox:1
      command: [sh, -c, "mkdir /www && echo web-ok >/www/index.html && exec httpd -f -p 8080 -h /www"]
      ports: [{containerPort: 8080}]
      startupProbe: {httpGet: {path: /, port: 8080}, periodSeconds: 1, failureThreshold: 30}
      readinessProbe: {tcpSocket: {port: 8080}, periodSeconds: 1}
`)
	k.apply(server + `---
apiVersion: v1
kind: Service
metadata: {name: web}
spec:
  selector: {app: web}
  ports: [{port: 80, targetPort: 8080}]
`)
	// Ready needs both the HTTP startup probe and the TCP readiness probe to pass.
	k.run("", "-n", k.Namespace, "wait", "pod/web", "--for=condition=Ready", "--timeout=60s")

	// DNS has its own test, so the client uses addresses.
	svc := k.run("", "-n", k.Namespace, "get", "service", "web", "-o", "jsonpath={.spec.clusterIP}")
	ip := k.run("", "-n", k.Namespace, "get", "pod", "web", "-o", "jsonpath={.status.podIP}")
	k.apply(pod("client", "  restartPolicy: Never\n", `      image: sbxr-test/busybox:1
      command: [sh, -c, "wget -qO- http://`+svc+` && wget -qO- http://`+ip+`:8080"]
`))
	k.run("", "-n", k.Namespace, "wait", "pod/client", "--for=jsonpath={.status.phase}=Succeeded", "--timeout=60s")

	if out := k.run("", "-n", k.Namespace, "logs", "client"); out != "web-ok\nweb-ok" {
		t.Errorf("client output %q, want web-ok through the Service and the pod IP", out)
	}

	pf := exec.CommandContext(t.Context(), "kubectl", "--context", k.Context, "-n", k.Namespace, "port-forward", "pod/web", ":8080")
	stdout, _ := pf.StdoutPipe()

	err := pf.Start()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = pf.Process.Kill(); _ = pf.Wait() }()

	line := make([]byte, 200)
	n, _ := stdout.Read(line)

	port := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(string(line[:n]))
	if port == nil {
		t.Fatalf("port-forward output %q", line[:n])
	}

	body := httpGet(t, "http://127.0.0.1:"+port[1])
	if body != "web-ok\n" {
		t.Errorf("port-forward body %q", body)
	}

	k.run("", "-n", k.Namespace, "delete", "pod/web", "pod/client", "service/web", "--wait")
}

func testDNS(t *testing.T, k *kind) {
	t.Helper()

	const host = "kubernetes.default.svc.cluster.local"

	pods := map[string]string{
		"dns-go":    "      image: sbxr-test/dnsgo:1\n      args: [" + host + "]\n",
		"dns-glibc": "      image: sbxr-test/debian:1\n      command: [getent, hosts, " + host + "]\n",
		"dns-musl":  "      image: sbxr-test/alpine:1\n      command: [getent, hosts, " + host + "]\n",
	}

	// Phase 2 carries UDP, so busybox nslookup, which uses connected UDP sockets, works too.
	if k.Phase == 2 {
		pods["dns-busybox"] = "      image: sbxr-test/busybox:1\n      command: [nslookup, " + host + "]\n"
	}

	for name, container := range pods {
		k.apply(pod(name, "  restartPolicy: Never\n", container))
	}

	for _, name := range slices.Sorted(maps.Keys(pods)) {
		phase := waitDone(k, name)
		if logs := k.run("", "-n", k.Namespace, "logs", name); phase != "Succeeded" || !strings.Contains(logs, "10.96.0.1") {
			t.Errorf("%s: phase %s, output %q; want the kubernetes Service address", name, phase, logs)
		}
	}

	k.run("", append([]string{"-n", k.Namespace, "delete", "pod", "--wait"}, slices.Sorted(maps.Keys(pods))...)...)
}

func testAdmission(t *testing.T, k *kind) {
	t.Helper()

	create := func(spec, container string) (string, error) {
		return k.Kubectl(t.Context(), []byte(pod("adm", spec, "      image: sbxr-test/busybox:1\n"+container)),
			"-n", k.Namespace, "create", "--dry-run=server", "-f", "-", "-o", "jsonpath={.spec.dnsConfig}")
	}

	for name, c := range map[string][2]string{
		"privileged":       {"", "      securityContext: {privileged: true}\n"},
		"host network":     {"  hostNetwork: true\n", ""},
		"host PID":         {"  hostPID: true\n", ""},
		"host IPC":         {"  hostIPC: true\n", ""},
		"add capabilities": {"", "      securityContext: {capabilities: {add: [NET_ADMIN]}}\n"},
	} {
		_, err := create(c[0], c[1])
		if err == nil || !strings.Contains(err.Error(), "denied request") {
			t.Errorf("%s: error %v, want a denial", name, err)
		}
	}

	k.apply(pod("adm-debug", "", "      image: sbxr-test/busybox:1\n      command: [sleep, \"600\"]\n"))

	_, err := k.Kubectl(t.Context(), nil, "-n", k.Namespace, "debug", "adm-debug", "--image=sbxr-test/busybox:1", "--profile=sysadmin", "--", "true")
	if err == nil || !strings.Contains(err.Error(), "denied request") {
		t.Errorf("privileged kubectl debug: error %v, want a denial", err)
	}

	k.run("", "-n", k.Namespace, "delete", "pod", "adm-debug", "--wait")

	if k.Phase == 2 {
		// Phase 2 runs exec probes, sends SIGTERM, and carries DNS over UDP, so no policy warns or changes the pod.
		out, err := create("", "      readinessProbe: {exec: {command: [\"true\"]}}\n")
		if err != nil || out != "" {
			t.Errorf("phase 2 pod with an exec probe: dnsConfig %q, error %v; want no change", out, err)
		}

		return
	}

	for want, manifest := range map[string]string{
		"exec probes": pod("adm", "", "      image: sbxr-test/busybox:1\n      readinessProbe: {exec: {command: [\"true\"]}}\n"),
		"SIGTERM":     strings.Replace(pod("adm", "", "      image: sbxr-test/busybox:1\n"), "GracePeriodSeconds: 1", "GracePeriodSeconds: 30", 1),
	} {
		cmd := exec.CommandContext(t.Context(), "kubectl", "--context", k.Context, "-n", k.Namespace, "create", "--dry-run=server", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)

		out, err := cmd.CombinedOutput()
		if err != nil || !regexp.MustCompile("Warning: .*"+want).Match(out) {
			t.Errorf("%v, output %q; want a warning about %s", err, out, want)
		}
	}

	for spec, want := range map[string]string{
		"": `{"options":[{"name":"use-vc"}]}`,
		"  dnsConfig: {options: [{name: ndots, value: \"2\"}]}\n": `{"options":[{"name":"ndots","value":"2"},{"name":"use-vc"}]}`,
		"  dnsConfig: {options: [{name: use-vc}]}\n":              `{"options":[{"name":"use-vc"}]}`,
	} {
		out, err := create(spec, "")
		if err != nil || out != want {
			t.Errorf("dnsConfig %q: got %s %v, want %s", spec, out, err, want)
		}
	}
}

func testDeletion(t *testing.T, k *kind) {
	t.Helper()

	k.apply(strings.Replace(pod("del", "", "      image: sbxr-test/busybox:1\n      command: [sleep, \"600\"]\n"),
		"terminationGracePeriodSeconds: 1", "terminationGracePeriodSeconds: 3", 1))
	k.run("", "-n", k.Namespace, "wait", "pod/del", "--for=condition=Ready", "--timeout=60s")
	uid := k.run("", "-n", k.Namespace, "get", "pod", "del", "-o", "jsonpath={.metadata.uid}")

	start := time.Now()

	k.run("", "-n", k.Namespace, "delete", "pod", "del", "--wait")

	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("deletion took %s with a grace period of 3s", took)
	}

	err := scenario.NoPodProcesses(t.Context(), k.Node, uid)
	if err != nil {
		t.Error(err)
	}
}

func testAgentSandbox(t *testing.T, k *kind) {
	t.Helper()

	manifest := httpGet(t, agentSandboxURL)
	if sum := sha256.Sum256([]byte(manifest)); hex.EncodeToString(sum[:]) != agentSandboxSHA256 {
		t.Fatalf("agent-sandbox manifest SHA-256 %x, want %s", sum, agentSandboxSHA256)
	}

	k.run(manifest, "apply", "--server-side", "-f", "-")
	k.run("", "-n", "agent-sandbox-system", "rollout", "status", "deploy/agent-sandbox-controller", "--timeout=2m")
	k.run("", "wait", "crd/sandboxes.agents.x-k8s.io", "--for=condition=Established", "--timeout=60s")

	k.apply(`apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: {name: box}
spec:
  podTemplate:
    spec:
      runtimeClassName: sbxr
      terminationGracePeriodSeconds: 1
      containers:
        - name: c
          image: sbxr-test/busybox:1
          imagePullPolicy: IfNotPresent
          command: [sh, -c, "echo sandbox-ok; uname -r; sleep 600"]
`)
	defer k.run("", "-n", k.Namespace, "delete", "sandbox", "box", "--wait")

	k.run("", "-n", k.Namespace, "wait", "pod/box", "--for=create", "--timeout=60s")
	k.run("", "-n", k.Namespace, "wait", "pod/box", "--for=condition=Ready", "--timeout=2m")

	if rc := k.run("", "-n", k.Namespace, "get", "pod", "box", "-o", "jsonpath={.spec.runtimeClassName}"); rc != "sbxr" {
		t.Errorf("sandbox pod runtimeClassName %q", rc)
	}

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(k.run("", "-n", k.Namespace, "logs", "box"), "sandbox-ok") {
		if time.Now().After(deadline) {
			t.Fatal("sandbox workload printed nothing")
		}

		time.Sleep(time.Second)
	}
}

// testOverhead compares the container cgroup memory with the memory that the guest uses (open question Q2).
func testOverhead(t *testing.T, k *kind) {
	t.Helper()

	overhead := k.run("", "get", "runtimeclass", "sbxr", "-o", "jsonpath={.overhead.podFixed.memory}")
	limit, _ := strconv.Atoi(strings.TrimSuffix(overhead, "Mi"))

	for _, guestMiB := range []int{256, 2048} {
		name := fmt.Sprintf("overhead-%d", guestMiB)
		k.apply(pod(name, "", fmt.Sprintf(`      image: sbxr-test/busybox:1
      command: [sh, -c, "sleep 3; grep -E 'MemTotal|MemFree' /proc/meminfo; sleep 600"]
      resources: {limits: {memory: %dMi}}
`, guestMiB)))
	}

	defer k.run("", "-n", k.Namespace, "delete", "pod", "overhead-256", "overhead-2048", "--wait")

	for _, guestMiB := range []int{256, 2048} {
		name := fmt.Sprintf("overhead-%d", guestMiB)
		k.run("", "-n", k.Namespace, "wait", "pod/"+name, "--for=condition=Ready", "--timeout=60s")

		var guest map[string]int

		for range 30 {
			guest = meminfo(k.run("", "-n", k.Namespace, "logs", name))
			if len(guest) == 2 {
				break
			}

			time.Sleep(time.Second)
		}

		// Phase 1 runs the VM in the container cgroup. Phase 2 runs it in the sbxr-vm cgroup under the pod cgroup.
		script := `cat /sys/fs/cgroup$(cut -d: -f3 /proc/$(crictl inspect -o go-template --template '{{.info.pid}}' ` +
			strings.TrimPrefix(k.run("", "-n", k.Namespace, "get", "pod", name, "-o", "jsonpath={.status.containerStatuses[0].containerID}"), "containerd://") +
			`)/cgroup)/memory.current`
		if k.Phase == 2 {
			uid := strings.ReplaceAll(k.run("", "-n", k.Namespace, "get", "pod", name, "-o", "jsonpath={.metadata.uid}"), "-", "_")
			script = `cat /sys/fs/cgroup/kubepods.slice/*/*pod` + uid + `.slice/sbxr-vm/memory.current`
		}

		current, _ := strconv.Atoi(k.node(script))

		used := (guest["MemTotal"] - guest["MemFree"]) / 1024
		vmm := current>>20 - used

		t.Logf("guest memory %d MiB, guest used %d MiB, container cgroup %d MiB, overhead %d MiB",
			guest["MemTotal"]/1024, used, current>>20, vmm)

		if len(guest) != 2 || vmm <= 0 || vmm > limit {
			t.Errorf("guest %d MiB: measured overhead %d MiB, RuntimeClass overhead %s", guestMiB, vmm, overhead)
		}
	}
}

func meminfo(logs string) map[string]int {
	m := map[string]int{}

	for line := range strings.Lines(logs) {
		f := strings.Fields(line)
		if len(f) == 3 {
			v, _ := strconv.Atoi(f[1])
			m[strings.TrimSuffix(f[0], ":")] = v
		}
	}

	return m
}

func testUninstall(t *testing.T, k *kind) {
	t.Helper()

	uninstall(k, k.manifests()+"/sbxr-uninstall.yaml")

	if got := k.run("", "get", "node", k.Node, "-o", `jsonpath={.metadata.labels.sbxr\.dev/ready}{.metadata.annotations.sbxr\.dev/preflight}`); got != "" {
		t.Errorf("node label or annotation remains: %q", got)
	}

	if files := k.node("ls -d /opt/sbxr /etc/containerd/conf.d/sbxr.toml /var/lib/sbxr/restart-pending /run/sbxr 2>/dev/null || true"); files != "" {
		t.Errorf("files remain on the node: %s", files)
	}

	objs := k.run("", "get", "runtimeclass,validatingadmissionpolicy,validatingadmissionpolicybinding,mutatingadmissionpolicy,mutatingadmissionpolicybinding,clusterrole,clusterrolebinding,namespace",
		"-o", "name")
	for obj := range strings.Lines(objs) {
		if _, name, _ := strings.Cut(strings.TrimSpace(obj), "/"); strings.HasPrefix(name, "sbxr") && name != k.Namespace {
			t.Errorf("object remains: %s", obj)
		}
	}
}

func testPreflightFailure(t *testing.T, k *kind) {
	t.Helper()

	manifests := k.manifests()

	// Remove the import of the drop-in directory, and restore it at the end.
	k.node("cp /etc/containerd/config.toml /etc/containerd/config.toml.sbxr-test && sed -i '/^imports/d' /etc/containerd/config.toml && systemctl restart containerd")
	defer k.node("mv /etc/containerd/config.toml.sbxr-test /etc/containerd/config.toml && systemctl restart containerd")

	before := k.node("sha256sum /etc/containerd/config.toml")

	k.run("", "apply", "-f", manifests+"/sbxr-install.yaml")
	k.run("", "-n", "sbxr-system", "rollout", "status", "ds/sbxr-node", "--timeout=8m")

	reason := k.run("", "get", "node", k.Node, "-o", `jsonpath={.metadata.annotations.sbxr\.dev/preflight}`)
	label := k.run("", "get", "node", k.Node, "-o", `jsonpath={.metadata.labels.sbxr\.dev/ready}`)

	if !strings.Contains(reason, "does not import /etc/containerd/conf.d/*.toml") || label != "" {
		t.Errorf("annotation %q, label %q; want the reason and no label", reason, label)
	}

	if after := k.node("sha256sum /etc/containerd/config.toml; ls /etc/containerd/conf.d 2>/dev/null || true"); after != before {
		t.Errorf("containerd config changed: %q, want %q", after, before)
	}

	uninstall(k, manifests+"/sbxr-uninstall.yaml")
}

// manifests renders the install and uninstall manifests for the installer image into a new directory.
func (k *kind) manifests() string {
	k.t.Helper()

	dir := k.t.TempDir()

	out, err := exec.CommandContext(k.t.Context(), "../../scripts/node-manifests.sh", k.image, dir, strconv.Itoa(k.Phase)).CombinedOutput()
	if err != nil {
		k.t.Fatalf("render manifests: %v: %s", err, out)
	}

	return dir
}

// uninstall applies the uninstall manifest, waits until every node is clean, and deletes it.
func uninstall(k *kind, manifest string) {
	k.t.Helper()

	k.run("", "apply", "-f", manifest)
	k.run("", "-n", "sbxr-system", "rollout", "status", "ds/sbxr-node", "--timeout=8m")
	k.run("", "delete", "-f", manifest, "--wait")
}

// waitDone waits until the pod name succeeds or fails and returns its phase.
func waitDone(k *kind, name string) string {
	k.t.Helper()

	for range 120 {
		phase := k.run("", "-n", k.Namespace, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		if phase == "Succeeded" || phase == "Failed" {
			return phase
		}

		time.Sleep(500 * time.Millisecond)
	}

	k.t.Fatalf("pod %s did not finish", name)

	return ""
}

func httpGet(t *testing.T, url string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// kindBase is the busybox image that the scenario images build on, as in the hv tests.
const kindBase = "mirror.gcr.io/library/busybox:1.37.0"

// kindImages builds the scenario images, loads them into the cluster, and returns their names there.
func kindImages(t *testing.T, cluster string) map[string]string {
	t.Helper()

	base, err := remote.Image(name.MustParseReference(kindBase), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatal(err)
	}

	served, stop, err := scenario.ServeImages(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Nothing listens on port 1, so the pull fails at once.
	images := map[string]string{scenario.MissingImage: "localhost:1/sbxr/missing:1"}
	dir := t.TempDir()

	for image, ref := range served {
		if image == scenario.MissingImage {
			continue
		}

		images[image] = "sbxr-scenario/" + image + ":1"
		archive := filepath.Join(dir, image+".tar")

		src, err := name.ParseReference(ref)

		var dst name.Tag
		if err == nil {
			dst, err = name.NewTag(images[image])
		}

		var img v1.Image
		if err == nil {
			img, err = remote.Image(src)
		}

		if err == nil {
			err = tarball.WriteToFile(archive, dst, img)
		}

		if err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "kind", "load", "image-archive", "--name", cluster, archive).CombinedOutput()
		if err != nil {
			t.Fatalf("kind load %s: %v: %s", archive, err, out)
		}
	}

	return images
}
