package run

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
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
		want          string
		wantInterrupt bool
	}{
		{name: "a ready guest gets Ctrl-C", busy: false, want: "a\x03b", wantInterrupt: false},
		{name: "a busy guest gets no Ctrl-C", busy: true, want: "a", wantInterrupt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				got         bytes.Buffer
				interrupted atomic.Bool
			)

			// The first read holds only "a", so a busy guest still gets the input before the Ctrl-C.
			src := iotest.OneByteReader(strings.NewReader("a\x03b"))
			busy := func() bool { return tt.busy }

			copyInput(&got, src, busy, func() { interrupted.Store(true) })

			if got.String() != tt.want || interrupted.Load() != tt.wantInterrupt {
				t.Errorf("copyInput() wrote %q, interrupt %v; want %q, %v", got.String(), interrupted.Load(), tt.want, tt.wantInterrupt)
			}
		})
	}
}

func TestConsoleWatchBusy(t *testing.T) {
	t.Parallel()

	var busyInWrite bool

	c := &consoleWatch{w: nil, started: atomic.Bool{}, writing: atomic.Bool{}}
	c.w = writerFunc(func(p []byte) (int, error) {
		busyInWrite = c.busy()

		return len(p), nil
	})

	busyBefore := c.busy()
	_, _ = c.Write([]byte("x"))

	if !busyBefore || !busyInWrite || c.busy() {
		t.Errorf("busy() before output, during a write, after it = %v, %v, %v; want true, true, false", busyBefore, busyInWrite, c.busy())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
