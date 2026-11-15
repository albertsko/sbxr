//go:build kind

package scenario_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/albertsko/sbxr/internal/scenario"
)

// phase2Tests check the PRD "Kubernetes, phase 2" criteria.
var phase2Tests = map[string]func(*testing.T, *kind){
	"two containers share one VM and localhost": testOneVMPerPod,
	"the guest owns the pod IP":                 testPodIP,
	"kubectl exec and exec probes":              testExec,
	"deletion sends SIGTERM":                    testGracefulStop,
	"a missing command is a start error":        testStartError,
	"a VM that cannot boot is stopped":          testBootFailure,
}

func testOneVMPerPod(t *testing.T, k *kind) {
	t.Helper()

	k.apply(`apiVersion: v1
kind: Pod
metadata: {name: two}
spec:
  runtimeClassName: sbxr
  terminationGracePeriodSeconds: 1
  containers:
    - name: web
      image: sbxr-test/busybox:1
      imagePullPolicy: IfNotPresent
      command: [sh, -c, "mkdir /www && echo local-ok >/www/index.html && exec httpd -f -p 127.0.0.1:8080 -h /www"]
    - name: client
      image: sbxr-test/busybox:1
      imagePullPolicy: IfNotPresent
      command: [sh, -c, "until wget -qO- http://127.0.0.1:8080; do sleep 0.2; done; exec sleep 600"]
`)
	defer k.run("", "-n", k.Namespace, "delete", "pod", "two", "--wait")

	k.run("", "-n", k.Namespace, "wait", "pod/two", "--for=condition=Ready", "--timeout=60s")
	waitLog(k, "two", "client", "local-ok")

	uid := k.run("", "-n", k.Namespace, "get", "pod", "two", "-o", "jsonpath={.metadata.uid}")
	if n := k.node(`grep -lsE "pod(` + uid + `|` + strings.ReplaceAll(uid, "-", "_") + `)" /proc/[0-9]*/cgroup | wc -l`); n != "1" {
		t.Errorf("%s processes in the pod cgroup, want 1 VM process", n)
	}
}

func testPodIP(t *testing.T, k *kind) {
	t.Helper()

	k.apply(pod("guest-ip", "", `      image: sbxr-test/busybox:1
      command: [sh, -c, "nc -lk -p 9000 -e echo tcp-ok & while true; do nc -lu -p 9001 -e echo udp-ok; done"]
`))
	defer k.run("", "-n", k.Namespace, "delete", "pod", "guest-ip", "--wait")

	k.run("", "-n", k.Namespace, "wait", "pod/guest-ip", "--for=condition=Ready", "--timeout=60s")
	ip := k.run("", "-n", k.Namespace, "get", "pod", "guest-ip", "-o", "jsonpath={.status.podIP}")

	if addr := k.run("", "-n", k.Namespace, "exec", "guest-ip", "--", "ip", "-4", "addr", "show", "eth0"); !strings.Contains(addr, ip+"/") {
		t.Errorf("guest eth0 %q, want the pod IP %s", addr, ip)
	}

	// The client runs with runc, outside any VM.
	k.apply(strings.Replace(pod("guest-ip-client", "  restartPolicy: Never\n", `      image: sbxr-test/busybox:1
      command: [sh, -c, "ping -c 1 -W 5 `+ip+` && echo icmp-ok; echo x | nc -w 5 `+ip+` 9000; echo x | nc -u -w 2 `+ip+` 9001"]
`), "  runtimeClassName: sbxr\n", "", 1))
	defer k.run("", "-n", k.Namespace, "delete", "pod", "guest-ip-client", "--wait")

	waitDone(k, "guest-ip-client")

	logs := k.run("", "-n", k.Namespace, "logs", "guest-ip-client")
	for _, want := range []string{"icmp-ok", "tcp-ok", "udp-ok"} {
		if !strings.Contains(logs, want) {
			t.Errorf("client output %q lacks %s", logs, want)
		}
	}
}

func testExec(t *testing.T, k *kind) {
	t.Helper()

	k.apply(pod("exec", "", `      image: sbxr-test/busybox:1
      command: [sh, -c, "touch /tmp/ready; exec sleep 600"]
      readinessProbe: {exec: {command: [cat, /tmp/ready]}, periodSeconds: 1}
`))
	defer k.run("", "-n", k.Namespace, "delete", "pod", "exec", "--wait")

	// Ready needs the exec probe to pass.
	k.run("", "-n", k.Namespace, "wait", "pod/exec", "--for=condition=Ready", "--timeout=60s")

	cmd := exec.CommandContext(t.Context(), "kubectl", "--context", k.Context, "-n", k.Namespace, "exec", "-i", "exec", "--",
		"sh", "-c", "cat; echo err >&2; exit 3")
	cmd.Stdin = strings.NewReader("from-stdin\n")

	var stdout, stderr strings.Builder

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	if code := cmd.ProcessState.ExitCode(); code != 3 || stdout.String() != "from-stdin\n" || !strings.Contains(stderr.String(), "err") {
		t.Errorf("kubectl exec: exit %d (%v), stdout %q, stderr %q; want 3, the stdin, and err on stderr", code, err, stdout.String(), stderr.String())
	}
}

func testGracefulStop(t *testing.T, k *kind) {
	t.Helper()

	k.apply(strings.Replace(pod("term", "", `      image: sbxr-test/busybox:1
      command: [sh, -c, "trap 'exit 0' TERM; echo ready; while true; do sleep 0.2; done"]
`), "GracePeriodSeconds: 1", "GracePeriodSeconds: 30", 1))
	k.apply(strings.Replace(pod("noterm", "", "      image: sbxr-test/busybox:1\n      command: [sleep, \"600\"]\n"),
		"GracePeriodSeconds: 1", "GracePeriodSeconds: 4", 1))

	for _, name := range []string{"term", "noterm"} {
		k.run("", "-n", k.Namespace, "wait", "pod/"+name, "--for=condition=Ready", "--timeout=60s")
	}

	waitLog(k, "term", "c", "ready")

	for _, c := range []struct {
		name     string
		min, max time.Duration
	}{
		{"term", 0, 10 * time.Second},                 // The workload exits on SIGTERM, long before the 30 s grace period.
		{"noterm", 3 * time.Second, 15 * time.Second}, // sleep as PID 1 ignores SIGTERM, so the VM stops after the 4 s grace period.
	} {
		uid := k.run("", "-n", k.Namespace, "get", "pod", c.name, "-o", "jsonpath={.metadata.uid}")
		start := time.Now()

		k.run("", "-n", k.Namespace, "delete", "pod", c.name, "--wait")

		if took := time.Since(start); took < c.min || took > c.max {
			t.Errorf("%s: deletion took %s, want %s to %s", c.name, took, c.min, c.max)
		}

		if err := scenario.NoPodProcesses(t.Context(), k.Node, uid); err != nil {
			t.Error(err)
		}
	}
}

func testStartError(t *testing.T, k *kind) {
	t.Helper()

	k.apply(pod("missing", "  restartPolicy: Never\n", "      image: sbxr-test/busybox:1\n      command: [nope]\n"))
	k.apply(pod("exit127", "  restartPolicy: Never\n", "      image: sbxr-test/busybox:1\n      command: [sh, -c, \"exit 127\"]\n"))

	defer k.run("", "-n", k.Namespace, "delete", "pod", "missing", "exit127", "--wait")

	waitDone(k, "missing")
	waitDone(k, "exit127")

	reason := func(name string) string {
		return k.run("", "-n", k.Namespace, "get", "pod", name, "-o", "jsonpath={.status.containerStatuses[0].state.terminated.reason} {.status.containerStatuses[0].state.terminated.exitCode}")
	}

	if got := reason("missing"); !strings.HasPrefix(got, "StartError ") {
		t.Errorf("missing command: %q, want a StartError", got)
	}

	if got := reason("exit127"); got != "Error 127" {
		t.Errorf("exit 127: %q, want Error 127", got)
	}
}

// testBootFailure starts a pod whose VM has too little memory to boot. The shim stops the VM within the boot timeout.
func testBootFailure(t *testing.T, k *kind) {
	t.Helper()

	k.apply(pod("noboot", "", "      image: sbxr-test/busybox:1\n      command: [sleep, \"600\"]\n      resources: {limits: {memory: 16Mi}}\n"))
	uid := k.run("", "-n", k.Namespace, "get", "pod", "noboot", "-o", "jsonpath={.metadata.uid}")

	deadline := time.Now().Add(90 * time.Second)

	for {
		events := k.run("", "-n", k.Namespace, "get", "events", "--field-selector", "involvedObject.name=noboot", "-o", "jsonpath={.items[*].message}")
		if strings.Contains(events, "guest agent") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("no sandbox error within 90 s; events: %s", events)
		}

		time.Sleep(2 * time.Second)
	}

	k.run("", "-n", k.Namespace, "delete", "pod", "noboot", "--wait")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := scenario.NoPodProcesses(ctx, k.Node, uid); err != nil {
		t.Error(err)
	}
}

// waitLog waits until the logs of container c in pod name contain want.
func waitLog(k *kind, name, c, want string) {
	k.t.Helper()

	for range 60 {
		if out, _ := k.Kubectl(k.t.Context(), nil, "-n", k.Namespace, "logs", name, "-c", c); strings.Contains(out, want) {
			return
		}

		time.Sleep(time.Second)
	}

	k.t.Fatalf("logs of %s/%s lack %q", name, c, want)
}
