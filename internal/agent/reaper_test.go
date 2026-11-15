//go:build linux

package agent

import (
	"testing"
	"testing/synctest"
	"time"
)

// TestReaperPIDReuse checks that a process never gets the exit of an earlier process with the same pid.
func TestReaperPIDReuse(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := newReaper()

		// An orphan that nobody subscribed to exits with 23. Its pid then goes to a new process.
		r.dispatch(42, exit{Status: 23, At: time.Now()})

		sub := r.subscribe()
		ch := sub.wait(42)

		synctest.Wait()

		select {
		case e := <-ch:
			t.Fatalf("the new process got exit %d of the earlier one", e.Status)
		default:
		}

		r.dispatch(42, exit{Status: 0, At: time.Now()})

		if e := <-ch; e.Status != 0 {
			t.Errorf("exit %d, want 0", e.Status)
		}
	})
}

// TestReaperEarlyExit checks that a process that exits before its pid is known still reports its own exit,
// the latest one for that pid.
func TestReaperEarlyExit(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := newReaper()
		sub := r.subscribe()

		r.dispatch(7, exit{Status: 1, At: time.Now()}) // An earlier process with pid 7.
		r.dispatch(8, exit{Status: 2, At: time.Now()})
		r.dispatch(7, exit{Status: 3, At: time.Now()}) // The subscribed process.

		if e := <-sub.wait(7); e.Status != 3 {
			t.Errorf("exit %d, want 3", e.Status)
		}

		// A process that exits after wait also ends its subscription.
		late := r.subscribe()
		ch := late.wait(9)

		r.dispatch(9, exit{Status: 4, At: time.Now()})

		if e := <-ch; e.Status != 4 {
			t.Errorf("late exit %d, want 4", e.Status)
		}

		synctest.Wait()

		r.mu.Lock()
		n := len(r.subs)
		r.mu.Unlock()

		if n != 0 {
			t.Errorf("%d finished subscriptions stay, want none", n)
		}
	})
}
