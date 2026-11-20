//go:build vm

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestVMNextRunRemovesStaleRunState(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]
	cacheDir := vmCacheDir(t)
	cmd, runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

	// Killing the runner and the stopped VM process leaves no process that could clean up.
	for _, pid := range []int{runner, vmPID} {
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err != nil {
			t.Fatal(err)
		}
	}

	// On Linux, the user namespace wrapper exits when the runner dies.
	waitExit(t, cmd)

	waitForNoProcess(t, cacheDir)

	if checkClean(cacheDir) == nil {
		t.Fatal("no run state remains after SIGKILL, so the test checks nothing")
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), runArgs(busybox, "true")...); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	err := checkClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestVMLiveVMKeepsItsRunState checks that a VM process that outlives its runner keeps its run root and the cache.
func TestVMLiveVMKeepsItsRunState(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]
	cacheDir := vmCacheDir(t)
	cmd, runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

	// The stopped VM process cannot see its lifeline close, so it stays alive with its run state.
	err := syscall.Kill(runner, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	// On Linux, the user namespace wrapper exits when the runner dies.
	waitExit(t, cmd)

	runs, err := filepath.Glob(filepath.Join(cacheDir, "runs", "*", "root"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("run roots = %q, %v; want the root of the stopped VM", runs, err)
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), runArgs(busybox, "true")...); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), "prune", "--force"); res.code != 1 || !strings.Contains(res.stderr, "a running sbxr run uses the image cache") {
		t.Errorf("prune = %d, stderr %q; want 1 and a busy cache", res.code, res.stderr)
	}

	if _, err := os.Stat(runs[0]); err != nil {
		t.Fatalf("the root of the stopped VM is gone: %v", err)
	}

	// Resumed, the VM process sees the closed lifeline and removes its run state.
	err = syscall.Kill(vmPID, syscall.SIGCONT)
	if err != nil {
		t.Fatal(err)
	}

	waitForNoProcess(t, cacheDir)

	err = checkClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// startStoppedVM starts a run, waits until its workload runs, and stops the VM process with SIGSTOP.
// It returns the started sbxr process and the process IDs of the runner and the VM process (see runnerOf).
func startStoppedVM(t *testing.T, bin, cacheDir, image string) (*exec.Cmd, int, int) {
	t.Helper()

	cmd := vmCommand(t, bin, vmEnv(cacheDir), runArgs(image, "sh", "-c", "echo ready; sleep 30")...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	readReady(t, cmd, stdout, readText(stdout, "ready\n"))

	pid := vmProcess(t, cacheDir)
	runner := runnerOf(t, cmd, pid)

	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	err = syscall.Kill(pid, syscall.SIGSTOP)
	if err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(t.Context(), psTimeout)
		state, _ := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()

		cancel()

		if strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
			return cmd, runner, pid
		}

		if time.Now().After(deadline) {
			t.Fatalf("the VM process is not stopped: state %q", state)
		}
	}
}
