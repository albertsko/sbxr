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

// reaper waits for every child of PID 1, including orphans that crun leaves, and hands each exit to the subscriptions
// that are active when it reaps the process. It keeps no exit that nobody subscribed to, so a reused pid never gets
// the exit of an earlier process. The agent never calls exec.Cmd.Wait, because the reaper's wait would race with it.
type reaper struct {
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

// subscription receives the exit of one process. Subscribe before the process can exit, then name its pid with wait.
type subscription struct {
	r *reaper

	mu  sync.Mutex
	pid int // Zero until wait names it.
	// early holds the exits that arrived before the pid was known, in reap order.
	early []pidExit
	done  chan exit
}

type pidExit struct {
	pid int
	e   exit
}

func newReaper() *reaper {
	return &reaper{mu: sync.Mutex{}, subs: map[*subscription]struct{}{}}
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

		r.dispatch(pid, exit{Status: exitStatus(ws), At: time.Now()})
	}
}

// dispatch hands the exit of pid to every active subscription. Without one, the exit is dropped.
func (r *reaper) dispatch(pid int, e exit) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for s := range r.subs {
		s.deliver(pid, e)
	}
}

// subscribe starts collecting exits for a process that is about to start.
func (r *reaper) subscribe() *subscription {
	s := &subscription{r: r, mu: sync.Mutex{}, pid: 0, early: nil, done: make(chan exit, 1)}

	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.mu.Unlock()

	return s
}

func (s *subscription) deliver(pid int, e exit) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.pid {
	case 0:
		s.early = append(s.early, pidExit{pid: pid, e: e})
	case pid:
		s.finish(e)
	}
}

// finish sends the exit once. The caller holds s.mu.
func (s *subscription) finish(e exit) {
	select {
	case s.done <- e:
	default:
	}

	s.pid = -1 // Nothing more to deliver.
	s.early = nil

	go s.close() // dispatch holds the reaper lock.
}

// wait names the process's pid and returns a channel that receives its exit, also when the process already exited.
// The latest early exit of pid counts: an earlier one belongs to a process that had the pid before.
func (s *subscription) wait(pid int) <-chan exit {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		last  exit
		found bool
	)

	for _, pe := range s.early {
		if pe.pid == pid {
			last, found = pe.e, true
		}
	}

	s.pid, s.early = pid, nil

	if found {
		s.finish(last)
	}

	return s.done
}

// close ends the subscription. A process that never started needs it.
func (s *subscription) close() {
	s.r.mu.Lock()
	delete(s.r.subs, s)
	s.r.mu.Unlock()
}

// start starts cmd and returns a channel that receives its exit.
func (r *reaper) start(cmd *exec.Cmd) (<-chan exit, error) {
	sub := r.subscribe()

	err := cmd.Start()
	if err != nil {
		sub.close()

		return nil, err //nolint:wrapcheck // Callers name the command.
	}

	return sub.wait(cmd.Process.Pid), nil
}

// exitStatus converts a wait status to a shell-style exit status.
func exitStatus(ws unix.WaitStatus) uint32 {
	if ws.Signaled() {
		return 128 + uint32(ws.Signal()) //nolint:gosec // Signal numbers are small.
	}

	return uint32(ws.ExitStatus()) //nolint:gosec // Exit statuses are 0 to 255.
}
