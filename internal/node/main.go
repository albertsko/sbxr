package node

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// doneFile marks that the installer finished. The DaemonSet readiness probe checks it.
const doneFile = "/tmp/done"

// Main runs "install" or "uninstall" for the node in NODE_NAME, then waits until ctx ends.
// It returns the process exit code. A non-zero code makes the kubelet restart the pod, which retries.
func Main(ctx context.Context, args []string, log *slog.Logger) int {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall") {
		log.Error("usage: sbxr-node install|uninstall")

		return 2
	}

	api, err := InCluster(os.Getenv("NODE_NAME"))
	if err != nil {
		log.Error("connect to the API server", "error", err)

		return 1
	}

	in := &Installer{
		API:      api,
		Host:     nsenter,
		HostRoot: "/host",
		Bundle:   "/bundle",
		CRI:      criHandlers,
		Poll:     2 * time.Second,
		// A healthy containerd serves the CRI within seconds of a restart. Units that do not report readiness to
		// systemd, such as k0s and k3s-agent, are "active" before containerd starts, so the wait covers the startup.
		Timeout:        5 * time.Minute,
		Log:            log,
		Proc:           "/proc",
		afterSubidRead: nil,
	}

	// Finish the host changes even when the pod is stopped meanwhile.
	work := context.WithoutCancel(ctx)
	if args[0] == "install" {
		err = in.Install(work)
	} else {
		err = in.Uninstall(work)
	}

	if err != nil {
		log.Error(args[0]+" failed", "error", err)

		return 1
	}

	err = os.WriteFile(doneFile, nil, 0o600)
	if err != nil {
		log.Error("mark done", "error", err)

		return 1
	}

	<-ctx.Done()

	return 0
}

// nsenter runs a command in the mount namespace of the host's PID 1.
func nsenter(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "nsenter", append([]string{"-t", "1", "-m", "--"}, args...)...).CombinedOutput() //nolint:gosec // The installer chooses every host command.
	if err != nil {
		return string(out), fmt.Errorf("%v: %w", args, err)
	}

	return string(out), nil
}
