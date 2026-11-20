//go:build vm

package e2e

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestVMPipedOutput(t *testing.T) {
	t.Parallel()

	bin := vmBinary(t)
	busybox := vmImages(t)[Busybox]

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
	}{
		{name: "workload exit code prints nothing", args: []string{busybox, "sh", "-c", "exit 3"}, wantCode: 3},
		{name: "flags after the image belong to the workload", args: []string{busybox, "echo", "--debug", "-e", "x"}, wantStdout: "--debug -e x\n"},
		{name: "separator after the image", args: []string{busybox, "--", "echo", "hi"}, wantStdout: "hi\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := sbxr(t, bin, vmEnv(vmCacheDir(t)), runArgs(tt.args...)...)
			if res.code != tt.wantCode || res.stdout != tt.wantStdout || res.stderr != "" {
				t.Errorf("sbxr run = %d, stdout %q, stderr %q; want %d, %q, and no stderr", res.code, res.stdout, res.stderr, tt.wantCode, tt.wantStdout)
			}
		})
	}
}

func TestVMGoneStdoutReaderStopsTheRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	cmd := vmCommand(t, vmBinary(t), vmEnv(cacheDir), runArgs(vmImages(t)[Busybox], "yes")...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		t.Fatal(err)
	}

	// Like head -1, the reader takes one line and goes away.
	readReady(t, cmd, stdout, readLines(bufio.NewReader(stdout), 1))

	_ = stdout.Close()

	if code := waitExit(t, cmd); code != 141 || stderr.Len() > 0 {
		t.Errorf("sbxr run yes = %d, stderr %q; want 141 and no sbxr error", code, stderr.String())
	}

	waitForNoProcess(t, cacheDir)

	err = checkClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

func TestVMGoneStdoutReaderStopsATTYRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	tm := startOnTerminal(t, vmBinary(t), vmEnv(cacheDir), pw, nil, runArgs("-q", "-t", vmImages(t)[Busybox], "yes")...)
	_ = pw.Close()

	readReady(t, tm.cmd, pr, readLines(bufio.NewReader(pr), 5))

	_ = pr.Close()

	if code := waitExit(t, tm.cmd); code != 141 || strings.Contains(tm.output(), "sbxr:") {
		t.Errorf("sbxr run -t yes = %d, terminal %q; want 141 and no sbxr error", code, tm.output())
	}

	waitForNoProcess(t, cacheDir)

	err = checkClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}

// TestVMGoneReaderOfDebugOutputStopsTheRun closes a pipe that carries both stdout and the debug logs on stderr.
// A later log write must not kill the runner before it cleans up.
func TestVMGoneReaderOfDebugOutputStopsTheRun(t *testing.T) {
	t.Parallel()

	cacheDir := vmCacheDir(t)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	cmd := vmCommand(t, vmBinary(t), vmEnv(cacheDir), append([]string{"--debug"}, runArgs(vmImages(t)[Busybox], "yes")...)...)
	cmd.Stdout, cmd.Stderr = pw, pw

	err = cmd.Start()
	_ = pw.Close()

	if err != nil {
		t.Fatal(err)
	}

	readReady(t, cmd, pr, readLines(bufio.NewReader(pr), 5))

	_ = pr.Close()

	if code := waitExit(t, cmd); code != 141 {
		t.Errorf("exit code = %d, want 141", code)
	}

	waitForNoProcess(t, cacheDir)

	err = checkClean(cacheDir)
	if err != nil {
		t.Error(err)
	}
}
