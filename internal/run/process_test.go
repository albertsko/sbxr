package run

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

func TestCopyUntil(t *testing.T) {
	t.Parallel()

	const marker = "MARKERX"

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "marker ends the copy", input: "hello" + marker + "after", want: "hello"},
		{name: "marker prefix in output", input: "MARK MA" + marker, want: "MARK MA"},
		{name: "read error before the marker", input: "partial MARK", want: "partial MARK"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got bytes.Buffer

			// One byte per read splits the marker across reads.
			copyUntil(&got, iotest.OneByteReader(bytes.NewReader([]byte(tt.input))), []byte(marker))

			if got.String() != tt.want {
				t.Errorf("copyUntil() wrote %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestCopyUntilDoesNotHoldBackOutput(t *testing.T) {
	t.Parallel()

	src, w := io.Pipe()
	got := make(chan string)
	dst := writerFunc(func(p []byte) (int, error) {
		got <- string(p)

		return len(p), nil
	})

	go copyUntil(dst, src, []byte("MARKER"))

	_, _ = w.Write([]byte("ready\n"))

	if s := <-got; s != "ready\n" {
		t.Errorf("first write = %q, want %q before more output", s, "ready\n")
	}

	_ = w.Close()
}

func TestCopyInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		started       bool
		wantInterrupt bool
	}{
		{name: "a ready guest gets Ctrl-C", started: true, wantInterrupt: false},
		{name: "a busy guest gets no Ctrl-C", started: false, wantInterrupt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var interrupted atomic.Bool

			got := make(chan string, 3)
			dst := writerFunc(func(p []byte) (int, error) {
				got <- string(p)

				return len(p), nil
			})

			// The first read holds only "a", so a busy guest still gets the input before the Ctrl-C.
			src := iotest.OneByteReader(strings.NewReader("a\x03b"))
			guest := &consoleWatch{w: nil, started: atomic.Bool{}, write: stallWatch{}}
			guest.started.Store(tt.started)

			copyInput(dst, src, guest, func() { interrupted.Store(true) })

			want := []string{"a", "\x03", "b"}
			if tt.wantInterrupt {
				want = want[:1]
			}

			for _, w := range want {
				if s := <-got; s != w {
					t.Errorf("copyInput() wrote %q, want %q", s, w)
				}
			}

			if interrupted.Load() != tt.wantInterrupt {
				t.Errorf("copyInput() interrupt = %v, want %v", interrupted.Load(), tt.wantInterrupt)
			}
		})
	}
}

func TestCopyInputStopsBehindStalledInput(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The guest terminal takes no input, like a guest that reads none after a long paste.
		w, entered, interrupted, stop := gatedCopyInput(func([]byte) bool { return true })
		defer stop()

		_, _ = w.Write([]byte("paste"))

		<-entered

		time.Sleep(stallAfter)

		_, _ = w.Write([]byte{ctrlC})

		synctest.Wait()

		if !interrupted.Load() {
			t.Error("copyInput() forwarded Ctrl-C behind input that the guest does not take")
		}
	})
}

func TestCopyInputRechecksAWriteThatStallsAfterTheRecheck(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The Ctrl-C passes, and the next input blocks from shortly before the first recheck.
		w, entered, interrupted, stop := gatedCopyInput(func(p []byte) bool { return p[0] != ctrlC })
		defer stop()

		_, _ = w.Write([]byte{ctrlC})

		time.Sleep(2*stallAfter - stallAfter/2)

		_, _ = w.Write([]byte("a"))

		<-entered

		time.Sleep(stallAfter + time.Millisecond)
		synctest.Wait()

		if !interrupted.Load() {
			t.Error("copyInput() kept the run going after the input stalled shortly after a Ctrl-C")
		}
	})
}

// gatedCopyInput runs copyInput on a ready guest and returns the input writer.
// Writes to the guest terminal for which block returns true signal entered and wait until stop.
func gatedCopyInput(block func([]byte) bool) (io.Writer, <-chan struct{}, *atomic.Bool, func()) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	interrupted := &atomic.Bool{}

	dst := writerFunc(func(p []byte) (int, error) {
		if block(p) {
			entered <- struct{}{}

			<-gate
		}

		return len(p), nil
	})
	src, w := io.Pipe()
	guest := &consoleWatch{w: nil, started: atomic.Bool{}, write: stallWatch{}}
	guest.started.Store(true)

	go copyInput(dst, src, guest, func() { interrupted.Store(true) })

	stop := func() {
		close(gate)

		_ = w.Close()
	}

	return w, entered, interrupted, stop
}

func TestConsoleWatchBusy(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var busyInWrite, busyInStall bool

		c := &consoleWatch{w: nil, started: atomic.Bool{}, write: stallWatch{}}
		c.w = writerFunc(func(p []byte) (int, error) {
			busyInWrite = c.busy()

			time.Sleep(stallAfter)

			busyInStall = c.busy()

			return len(p), nil
		})

		busyBefore := c.busy()
		_, _ = c.Write([]byte("x"))

		if !busyBefore || busyInWrite || !busyInStall || c.busy() {
			t.Errorf("busy() before output, in a write, in a stalled write, after it = %v, %v, %v, %v; want true, false, true, false",
				busyBefore, busyInWrite, busyInStall, c.busy())
		}
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
