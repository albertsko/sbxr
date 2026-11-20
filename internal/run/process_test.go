package run

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
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
		busy          bool
		wantInterrupt bool
	}{
		{name: "a ready guest gets Ctrl-C", busy: false, wantInterrupt: false},
		{name: "a busy guest gets no Ctrl-C", busy: true, wantInterrupt: true},
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
			busy := func() bool { return tt.busy }

			copyInput(dst, src, busy, func() { interrupted.Store(true) })

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

	// The guest terminal takes no input, like a guest that reads none after a long paste.
	blocked := make(chan struct{})
	defer close(blocked)

	dst := writerFunc(func(p []byte) (int, error) {
		<-blocked

		return len(p), nil
	})
	src, w := io.Pipe()
	interrupted := make(chan struct{})

	go copyInput(dst, src, func() bool { return false }, func() { close(interrupted) })

	_, _ = w.Write([]byte("paste"))

	time.Sleep(2 * stallAfter)

	_, _ = w.Write([]byte{ctrlC})

	select {
	case <-interrupted:
	case <-time.After(5 * time.Second):
		t.Error("copyInput() forwarded Ctrl-C behind input that the guest does not take")
	}
}

func TestConsoleWatchBusy(t *testing.T) {
	t.Parallel()

	var busyInWrite, busyInStall bool

	c := &consoleWatch{w: nil, started: atomic.Bool{}, write: stallWatch{}}
	c.w = writerFunc(func(p []byte) (int, error) {
		busyInWrite = c.busy()

		time.Sleep(2 * stallAfter)

		busyInStall = c.busy()

		return len(p), nil
	})

	busyBefore := c.busy()
	_, _ = c.Write([]byte("x"))

	if !busyBefore || busyInWrite || !busyInStall || c.busy() {
		t.Errorf("busy() before output, in a write, in a stalled write, after it = %v, %v, %v, %v; want true, false, true, false",
			busyBefore, busyInWrite, busyInStall, c.busy())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
