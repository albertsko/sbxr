package run

import (
	"bytes"
	"io"
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

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
