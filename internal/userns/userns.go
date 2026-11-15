//go:build linux

// Package userns runs sbxr inside a new user and mount namespace without root.
//
// The namespace maps uid and gid 0 to the current user and ids 1 to N to the user's subordinate range,
// so image files keep owners such as gid 42. One direct mapping cannot cover those ids,
// so the maps are written with the setuid helpers newuidmap and newgidmap.
//
// Entering takes two stages, both handled by [Init] at the start of main.
// The child of [Start] waits until the parent wrote the maps, then executes itself again,
// because only an execve as mapped uid 0 grants full capabilities in the namespace.
// The second stage makes all mounts private.
package userns

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	stageEnv    = "_SBXR_USERNS"
	stageWait   = "wait"
	stageEnter  = "enter"
	stageInside = "inside"

	// syncFD is the child fd of the pipe that reports the written maps, the first entry of exec.Cmd.ExtraFiles.
	syncFD = 3
)

var (
	// ErrNoHelpers means that newuidmap or newgidmap is missing.
	ErrNoHelpers = errors.New("newuidmap and newgidmap are not installed")
	// ErrNoSubIDs means that the user has no subordinate id range.
	ErrNoSubIDs = errors.New("no subordinate id range")
)

// Init finishes entering a namespace that [Start] created. Call it first in main.
// Outside a namespace, and in processes that started inside one, it does nothing.
func Init() error {
	switch os.Getenv(stageEnv) {
	case stageWait:
		return reexec()
	case stageEnter:
		return enter()
	default:
		return nil
	}
}

// Inside reports whether this process runs inside a namespace that [Start] created.
func Inside() bool {
	return os.Getenv(stageEnv) == stageInside
}

func reexec() error {
	sync := os.NewFile(syncFD, "userns-sync")

	var ok [1]byte

	_, err := io.ReadFull(sync, ok[:])
	if err != nil {
		return errors.New("the parent did not write the user namespace maps")
	}

	_ = sync.Close() // The fd must not reach the next stage.

	err = os.Setenv(stageEnv, stageEnter)
	if err != nil {
		return fmt.Errorf("set %s: %w", stageEnv, err)
	}

	err = syscall.Exec("/proc/self/exe", os.Args, os.Environ()) //nolint:gosec // The process executes itself with its own arguments.

	return fmt.Errorf("execute in user namespace: %w", err)
}

func enter() error {
	// The capabilities gained by the execve reset the parent death signal, so set it again.
	err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0)
	if err != nil {
		return fmt.Errorf("set parent death signal: %w", err)
	}

	if os.Getppid() == 1 {
		return errors.New("the parent exited")
	}

	err = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	if err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	err = os.Setenv(stageEnv, stageInside)
	if err != nil {
		return fmt.Errorf("set %s: %w", stageEnv, err)
	}

	return nil
}

// Start starts cmd, which must run this binary, inside a new user and mount namespace.
func Start(ctx context.Context, cmd *exec.Cmd) error {
	uids, err := subIDs("/etc/subuid")
	if err != nil {
		return err
	}

	gids, err := subIDs("/etc/subgid")
	if err != nil {
		return err
	}

	uidmap, err1 := exec.LookPath("newuidmap")
	gidmap, err2 := exec.LookPath("newgidmap")

	if err1 != nil || err2 != nil {
		return ErrNoHelpers
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create user namespace pipe: %w", err)
	}
	defer w.Close() //nolint:errcheck // Closing without a write tells the child to give up.

	cmd.ExtraFiles = append([]*os.File{r}, cmd.ExtraFiles...)
	cmd.Env = append(cmd.Environ(), stageEnv+"="+stageWait)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
	// SIGTERM lets a run clean up when the process outside dies.
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM

	err = cmd.Start()
	_ = r.Close() // Only the child reads the pipe.

	if err != nil {
		return fmt.Errorf("start process in user namespace: %w", err)
	}

	pid := strconv.Itoa(cmd.Process.Pid)

	err = errors.Join(
		writeMap(ctx, uidmap, pid, os.Getuid(), uids),
		writeMap(ctx, gidmap, pid, os.Getgid(), gids),
	)
	if err != nil {
		_ = cmd.Process.Kill() // The child waits on the pipe and has done nothing yet.
		_ = cmd.Wait()

		return err
	}

	_, err = w.Write([]byte{1})
	if err != nil {
		return fmt.Errorf("release user namespace process: %w", err)
	}

	return nil
}

// Run runs this binary with args inside a new user and mount namespace and returns its exit code.
// It forwards SIGINT, SIGTERM, and SIGHUP to the process.
func Run(args []string, stdin, stdout, stderr *os.File) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find the sbxr binary: %w", err)
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	defer signal.Stop(signals)

	cmd := exec.CommandContext(context.Background(), exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr

	err = Start(context.Background(), cmd)
	if err != nil {
		return 0, err
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig) // The process may have exited already.
			case <-done:
				return
			}
		}
	}()

	err = cmd.Wait()

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}

		return exit.ExitCode(), nil
	}

	if err != nil {
		return 0, fmt.Errorf("wait for process in user namespace: %w", err)
	}

	return 0, nil
}

// idRange is a subordinate id range from /etc/subuid or /etc/subgid.
type idRange struct{ start, count int }

func writeMap(ctx context.Context, helper, pid string, id int, sub idRange) error {
	out, err := exec.CommandContext(ctx, helper, pid,
		"0", strconv.Itoa(id), "1",
		"1", strconv.Itoa(sub.start), strconv.Itoa(sub.count),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", helper, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// subIDs returns the first range of the current user in path, matched by name or uid.
func subIDs(path string) (idRange, error) {
	uid := strconv.Itoa(os.Getuid())

	names := []string{uid}
	if u, err := user.LookupId(uid); err == nil {
		names = append(names, u.Username)
	}

	f, err := os.Open(path)
	if err != nil {
		return idRange{}, fmt.Errorf("%w for uid %s: %w", ErrNoSubIDs, uid, err)
	}
	defer f.Close() //nolint:errcheck // The file was only read.

	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Split(strings.TrimSpace(s.Text()), ":")
		if len(fields) != 3 || !slices.Contains(names, fields[0]) {
			continue
		}

		start, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])

		if err1 == nil && err2 == nil && start > 0 && count > 0 {
			return idRange{start: start, count: count}, nil
		}
	}

	return idRange{}, fmt.Errorf("%w for %s in %s", ErrNoSubIDs, strings.Join(names, " or "), path)
}
