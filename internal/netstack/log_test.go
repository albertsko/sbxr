package netstack

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	gvlog "gvisor.dev/gvisor/pkg/log"
)

//nolint:paralleltest // The library loggers are process-wide, and parallel tests wait until sequential tests end.
func TestLibraryLogsGoToDebugLogger(t *testing.T) {
	var buf bytes.Buffer

	routeLibraryLogs(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { routeLibraryLogs(slog.New(slog.DiscardHandler)) })

	if logrus.StandardLogger().Out != io.Discard {
		t.Error("logrus writes to its own output")
	}

	logrus.Error("tap failed")
	gvlog.Warningf("tcp %s", "warning")

	for _, want := range []string{"level=DEBUG msg=\"tap failed\"", "level=DEBUG msg=\"tcp warning\""} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("debug log %q does not contain %q", buf.String(), want)
		}
	}
}

// TestStartLogsPublishedPorts pins the debug line that `sbxr run --debug` prints and the scenario targets read.
func TestStartLogsPublishedPorts(t *testing.T) {
	t.Parallel()

	var buf syncBuffer

	n, err := Start(t.Context(), Config{
		Publish: []Publish{{Guest: 80}},
		Logger:  slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	want := `msg="published ` + n.Published()[0].Host.String() + ` -> 80/tcp"`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("debug log %q does not contain %q", buf.String(), want)
	}
}

// syncBuffer is a buffer that library goroutines may write while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // A buffer write does not fail.
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
