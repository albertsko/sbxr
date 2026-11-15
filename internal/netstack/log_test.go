package netstack

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
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
