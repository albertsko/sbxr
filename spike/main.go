//go:build linux

// Command spike checks a per-run overlayfs root for libkrun inside a rootless user namespace.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sbxr main|stage1|stage2|vm ...")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "main":
		err = runParent(os.Args[2:])
	case "stage1":
		err = runStage1(os.Args[2:])
	case "stage2":
		err = runChecks(os.Args[2:])
	case "vm":
		err = runVM(os.Args[2], os.Args[3:])
	default:
		err = fmt.Errorf("unknown stage %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sbxr:", err)
		os.Exit(125)
	}
}

func runParent(args []string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	subuid, err := subID("/etc/subuid", u.Username)
	if err != nil {
		return err
	}
	subgid, err := subID("/etc/subgid", u.Username)
	if err != nil {
		return err
	}

	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(exe, append([]string{"stage1"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start userns child: %w", err)
	}
	r.Close()

	pid := strconv.Itoa(cmd.Process.Pid)
	if out, err := exec.Command("newuidmap", pid, "0", u.Uid, "1", "1", subuid[0], subuid[1]).CombinedOutput(); err != nil {
		return fmt.Errorf("newuidmap: %v: %s", err, out)
	}
	if out, err := exec.Command("newgidmap", pid, "0", u.Gid, "1", "1", subgid[0], subgid[1]).CombinedOutput(); err != nil {
		return fmt.Errorf("newgidmap: %v: %s", err, out)
	}
	w.Close()

	err = cmd.Wait()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	return err
}

func subID(path, name string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.Split(s.Text(), ":")
		if len(parts) == 3 && parts[0] == name {
			return parts[1:], nil
		}
	}
	return nil, fmt.Errorf("no entry for %s in %s", name, path)
}

// The child execs again after the maps are written, because only an exec as mapped uid 0 grants full capabilities in the namespace.
func runStage1(args []string) error {
	pipe := os.NewFile(3, "sync")
	buf := make([]byte, 1)
	pipe.Read(buf)
	pipe.Close()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, append([]string{"sbxr", "stage2"}, args...), os.Environ())
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "vm" {
		runtime.LockOSThread()
	}
}
