//go:build kube

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kubeTarget is the kube target. It runs each workload as a pod with runtimeClassName sbxr in a kind cluster.
type kubeTarget struct {
	// Context is the kubectl context of the cluster.
	Context string
	// Node is the kind node container, where the cleanup check looks for VM processes.
	Node      string
	Namespace string
	// Images maps scenario image names to images in the cluster.
	Images map[string]string

	mu   sync.Mutex
	pods map[string]string // Pod name to UID.
}

// Capabilities returns the capabilities of the kube target.
func (k *kubeTarget) Capabilities() []capability {
	return []capability{Net, Publish, DNS, MemoryLimit, Probes, Exec, GracefulStop}
}

// Run creates a pod for w, waits until it ends or cannot start, and deletes it.
func (k *kubeTarget) Run(ctx context.Context, w workload) (runResult, error) {
	if w.Interactive || w.TTY || w.InterruptAfter != "" {
		return runResult{}, errors.New("the kube target has no stdin, tty, or signals")
	}

	name := "s-" + strings.ToLower(rand.Text()[:10])

	pod, err := podFor(name, k.Images[w.Image], w)
	if err != nil {
		return runResult{}, err
	}

	spec, err := json.Marshal(pod)
	if err != nil {
		return runResult{}, fmt.Errorf("encode pod: %w", err)
	}

	uid, err := k.kubectl(ctx, spec, "-n", k.Namespace, "create", "-f", "-", "-o", "jsonpath={.metadata.uid}")
	if err != nil && (strings.Contains(err.Error(), "(Invalid)") || strings.Contains(err.Error(), "(Forbidden)")) {
		// The API server rejects the pod before anything runs.
		return runResult{Stdout: "", Stderr: "", ExitCode: 125, Class: InvalidSpec}, nil
	}

	if err != nil {
		return runResult{}, err
	}

	k.mu.Lock()
	if k.pods == nil {
		k.pods = map[string]string{}
	}

	k.pods[name] = uid
	k.mu.Unlock()

	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()

		_, _ = k.kubectl(ctx, nil, "-n", k.Namespace, "delete", "pod", name, "--wait", "--grace-period=1") // CheckClean reports leftovers.
	}()

	return k.wait(ctx, name)
}

// wait polls the pod until its container terminates or fails to start.
func (k *kubeTarget) wait(ctx context.Context, name string) (runResult, error) {
	for {
		out, err := k.kubectl(ctx, nil, "-n", k.Namespace, "get", "pod", name, "-o", "jsonpath={.status.containerStatuses[0].state}")
		if err != nil {
			return runResult{}, err
		}

		var state struct {
			Terminated *struct {
				ExitCode int    `json:"exitCode"`
				Reason   string `json:"reason"`
				Message  string `json:"message"`
			} `json:"terminated"`
			Waiting *struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"waiting"`
		}

		if out != "" {
			err = json.Unmarshal([]byte(out), &state)
			if err != nil {
				return runResult{}, fmt.Errorf("decode container state %q: %w", out, err)
			}
		}

		switch {
		case state.Terminated != nil && state.Terminated.Reason == "StartError":
			// A command that cannot run is a start error, as with runc.
			return startError(state.Terminated.Message), nil
		case state.Terminated != nil:
			logs, err := k.kubectl(ctx, nil, "-n", k.Namespace, "logs", name)
			if err != nil {
				return runResult{}, err
			}

			return runResult{Stdout: logs, Stderr: "", ExitCode: state.Terminated.ExitCode, Class: classify(state.Terminated.ExitCode, "")}, nil
		case state.Waiting == nil:
		case state.Waiting.Reason == "ErrImagePull" || state.Waiting.Reason == "ImagePullBackOff" || state.Waiting.Reason == "InvalidImageName":
			return runResult{Stdout: "", Stderr: "", ExitCode: 125, Class: Image}, nil
		case strings.HasSuffix(state.Waiting.Reason, "Error"):
			return runResult{Stdout: state.Waiting.Message, Stderr: "", ExitCode: 125, Class: InvalidSpec}, nil
		}

		select {
		case <-ctx.Done():
			return runResult{}, fmt.Errorf("pod %s: %w (state %s)", name, ctx.Err(), out)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// startError maps a start error to the error class of a command that does not exist or cannot run.
func startError(msg string) runResult {
	switch {
	case strings.Contains(msg, "not found") || strings.Contains(msg, "No such file"):
		return runResult{Stdout: "", Stderr: msg, ExitCode: 127, Class: NotFound}
	case strings.Contains(msg, "ermission denied") || strings.Contains(msg, "not executable"):
		return runResult{Stdout: "", Stderr: msg, ExitCode: 126, Class: NotExecutable}
	}

	return runResult{Stdout: "", Stderr: msg, ExitCode: 125, Class: InvalidSpec}
}

// CheckClean reports pods of this target that still exist and processes that remain in their cgroups on the node.
func (k *kubeTarget) CheckClean() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	k.mu.Lock()
	defer k.mu.Unlock()

	var errs []error

	for name, uid := range k.pods {
		out, err := k.kubectl(ctx, nil, "-n", k.Namespace, "get", "pod", name, "--ignore-not-found", "-o", "name")
		if err != nil {
			errs = append(errs, err)
		} else if out != "" {
			errs = append(errs, fmt.Errorf("pod remains: %s", name))
		}

		err = noPodProcesses(ctx, k.Node, uid)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// noPodProcesses waits until no process on the kind node runs in a cgroup of the pod with uid.
func noPodProcesses(ctx context.Context, node, uid string) error {
	// Pod cgroups contain the UID with dashes (cgroupfs) or underscores (systemd).
	script := `grep -lsE "pod(` + uid + `|` + strings.ReplaceAll(uid, "-", "_") + `)" /proc/[0-9]*/cgroup || true`

	for {
		out, err := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", script).Output()
		if err == nil && len(bytes.TrimSpace(out)) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("processes of pod %s remain on the node: %s %w", uid, bytes.TrimSpace(out), err)
		case <-time.After(time.Second):
		}
	}
}

// kubectl runs kubectl with the target's context and stdin, and returns stdout.
func (k *kubeTarget) kubectl(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", k.Context}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}

	return string(out), nil
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// podFor returns the pod for w. Kubernetes expands $(NAME) and turns $$ into $, so every $ is doubled.
func podFor(name, image string, w workload) (map[string]any, error) {
	escape := func(s string) string { return strings.ReplaceAll(s, "$", "$$") }

	container := map[string]any{
		"name":            "c",
		"image":           image,
		"imagePullPolicy": "IfNotPresent",
	}

	if len(w.Args) > 0 {
		args := make([]string, len(w.Args))
		for i, a := range w.Args {
			args[i] = escape(a)
		}

		container["args"] = args
	}

	if len(w.Env) > 0 {
		env := make([]envVar, 0, len(w.Env))
		for _, kv := range w.Env {
			k, v, _ := strings.Cut(kv, "=")
			env = append(env, envVar{Name: k, Value: escape(v)})
		}

		container["env"] = env
	}

	if w.Workdir != "" {
		container["workingDir"] = w.Workdir
	}

	if w.Memory > 0 {
		container["resources"] = map[string]any{"limits": map[string]string{"memory": strconv.Itoa(w.Memory) + "Mi"}}
	}

	if w.User != "" {
		sc := map[string]int64{}

		for i, key := range []string{"runAsUser", "runAsGroup"} {
			ids := strings.Split(w.User, ":")
			if i >= len(ids) {
				break
			}

			id, err := strconv.ParseInt(ids[i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("the kube target needs numeric user ids, got %q", w.User)
			}

			sc[key] = id
		}

		container["securityContext"] = sc
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"runtimeClassName":              "sbxr",
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": 1,
			"enableServiceLinks":            false,
			"containers":                    []any{container},
		},
	}, nil
}
