//go:build vm

package e2e

import (
	"io"
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
	runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

	// Killing the runner and the stopped VM process leaves no process that could clean up.
	for _, pid := range []int{runner.Process.Pid, vmPID} {
		err := syscall.Kill(pid, syscall.SIGKILL)
		if err != nil {
			t.Fatal(err)
		}
	}

	_ = runner.Wait() // The process was killed.

	waitForNoProcess(t, cacheDir)

	if CheckClean(cacheDir) == nil {
		t.Fatal("no run state remains after SIGKILL, so the test checks nothing")
	}

	if res := sbxr(t, bin, vmEnv(cacheDir), runArgs(busybox, "true")...); res.code != 0 {
		t.Fatalf("next run = %d, stderr %q", res.code, res.stderr)
	}

	err := CheckClean(cacheDir)
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
	runner, vmPID := startStoppedVM(t, bin, cacheDir, busybox)

	// The stopped VM process cannot see its lifeline close, so it stays alive with its run state.
	err := syscall.Kill(runner.Process.Pid, syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}

	_ = runner.Wait() // The process was killed.

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

	err = CheckClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// startStoppedVM starts a run, waits until its workload runs, and stops the VM process with SIGSTOP.
// It returns the runner and the VM process ID.
func startStoppedVM(t *testing.T, bin, cacheDir, image string) (*exec.Cmd, int) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), bin, runArgs(image, "sh", "-c", "echo ready; sleep 30")...)
	cmd.Env = vmEnv(cacheDir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	_, err = io.ReadFull(stdout, make([]byte, len("ready\n")))
	if err != nil {
		t.Fatal(err)
	}

	pid := vmProcess(t, cacheDir)

	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	err = syscall.Kill(pid, syscall.SIGSTOP)
	if err != nil {
		t.Fatal(err)
	}

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		state, _ := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
			return cmd, pid
		}

		if time.Now().After(deadline) {
			t.Fatalf("the VM process is not stopped: state %q", state)
		}
	}
}
