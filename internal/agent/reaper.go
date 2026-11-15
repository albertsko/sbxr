//go:build linux

package agent

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// exit is how a process ended.
type exit struct {
	Status uint32
	At     time.Time
}

// reaper waits for every child of PID 1, including orphans that crun leaves, and hands each exit to its watcher.
// The agent never calls exec.Cmd.Wait, because the reaper's wait would race with it.
type reaper struct {
	mu       sync.Mutex
	watchers map[int]chan exit
	// unclaimed holds exits of processes that nobody watches yet, such as a container that exits before crun create returns.
	unclaimed map[int]exit
}

func newReaper() *reaper {
	return &reaper{mu: sync.Mutex{}, watchers: map[int]chan exit{}, unclaimed: map[int]exit{}}
}

func (r *reaper) run() {
	sig := make(chan os.Signal, 32)
	signal.Notify(sig, unix.SIGCHLD)

	for range sig {
		r.reap()
	}
}

func (r *reaper) reap() {
	for {
		var ws unix.WaitStatus

		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}

		e := exit{Status: exitStatus(ws), At: time.Now()}

		r.mu.Lock()

		if ch, ok := r.watchers[pid]; ok {
			delete(r.watchers, pid)

			ch <- e
		} else {
			r.unclaimed[pid] = e
		}

		r.mu.Unlock()
	}
}

// watch returns a channel that receives the exit of pid.
func (r *reaper) watch(pid int) <-chan exit {
	r.mu.Lock()
	defer r.mu.Unlock()

	ch := make(chan exit, 1)

	if e, ok := r.unclaimed[pid]; ok {
		delete(r.unclaimed, pid)

		ch <- e

		return ch
	}

	r.watchers[pid] = ch

	return ch
}

// start starts cmd and watches it. The lock keeps the reaper from taking the exit before the watch exists.
func (r *reaper) start(cmd *exec.Cmd) (<-chan exit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	err := cmd.Start()
	if err != nil {
		return nil, err //nolint:wrapcheck // Callers name the command.
	}

	ch := make(chan exit, 1)
	r.watchers[cmd.Process.Pid] = ch

	return ch, nil
}

// exitStatus converts a wait status to a shell-style exit status.
func exitStatus(ws unix.WaitStatus) uint32 {
	if ws.Signaled() {
		return 128 + uint32(ws.Signal()) //nolint:gosec // Signal numbers are small.
	}

	return uint32(ws.ExitStatus()) //nolint:gosec // Exit statuses are 0 to 255.
}
