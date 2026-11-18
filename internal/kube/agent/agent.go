//go:build linux

// Package agent is the sbxr guest agent, PID 1 of a pod VM.
//
// It applies the pod config, then serves the containerd task API on a vsock port and runs each container with crun.
// The shim on the host forwards containerd's task requests here and carries stdio over a second vsock port.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"

	"github.com/albertsko/sbxr/internal/kube/podvm"
)

// Main runs the agent. It returns only on a setup failure, which ends the VM.
func Main() int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	err := run(log)
	log.Error("agent stopped", "error", err)

	// PID 1 must not exit, so restart the VM, which ends it. The shim reports the failure.
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)

	return 1
}

func run(log *slog.Logger) error {
	err := mountSystem()
	if err != nil {
		return err
	}

	pod, err := podvm.ReadPod(podvm.GuestShare)
	if err != nil {
		return fmt.Errorf("pod config: %w", err)
	}

	err = setupPod(pod)
	if err != nil {
		return err
	}

	r := newReaper()
	go r.run()

	svc := newService(r, log)

	streams, err := listenVsock(podvm.StreamPort)
	if err != nil {
		return err
	}

	go svc.serveStreams(streams)

	ctl, err := listenVsock(podvm.ControlPort)
	if err != nil {
		return err
	}

	srv, err := ttrpc.NewServer()
	if err != nil {
		return fmt.Errorf("create ttrpc server: %w", err)
	}

	taskAPI.RegisterTTRPCTaskService(srv, svc)
	log.Info("agent ready", "hostname", pod.Hostname, "addresses", pod.Addresses)

	err = srv.Serve(context.Background(), ctl)

	return errors.Join(errors.New("control server ended"), err)
}

const tmpfs = "tmpfs"

// mountSystem mounts what the libkrun init leaves out, and the pod share.
func mountSystem() error {
	// Container roots live on the share, so it keeps setuid bits.
	mounts := []struct {
		src, dst, fstype string
		flags            uintptr
		data             string
	}{
		{tmpfs, "/run", tmpfs, unix.MS_NOSUID | unix.MS_NODEV, ""},
		{tmpfs, "/tmp", tmpfs, unix.MS_NOSUID | unix.MS_NODEV, ""},
		{"shm", podvm.GuestShm, tmpfs, unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, "mode=1777,size=" + podvm.GuestShmSize},
		{podvm.ShareTag, podvm.GuestShare, "virtiofs", 0, ""},
	}

	for _, m := range mounts {
		err := os.MkdirAll(m.dst, 0o755) //nolint:gosec // System directories of the guest.
		if err == nil {
			err = unix.Mount(m.src, m.dst, m.fstype, m.flags, m.data)
		}

		if err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.dst, err)
		}
	}

	return nil
}
