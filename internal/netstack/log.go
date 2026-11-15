package netstack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	gvlog "gvisor.dev/gvisor/pkg/log"
)

var (
	// libLogger receives the log output of gvisor-tap-vsock and gvisor, which use process-wide loggers.
	libLogger    atomic.Pointer[slog.Logger]
	routeLibLogs sync.Once
)

// routeLibraryLogs sends library log output to logger at debug level, so it never reaches the terminal unless debug logging is on.
func routeLibraryLogs(logger *slog.Logger) {
	libLogger.Store(logger)

	routeLibLogs.Do(func() {
		logrus.SetOutput(io.Discard)
		logrus.AddHook(logrusHook{})
		gvlog.SetTarget(gvisorEmitter{})
	})

	level := logrus.InfoLevel
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		level = logrus.DebugLevel
	}

	logrus.SetLevel(level)
}

type logrusHook struct{}

func (logrusHook) Levels() []logrus.Level { return logrus.AllLevels }

func (logrusHook) Fire(e *logrus.Entry) error {
	libLogger.Load().Debug(e.Message, "lib", "gvisor-tap-vsock", "level", e.Level.String())

	return nil
}

type gvisorEmitter struct{}

//nolint:goprintffuncname // Emit implements the gvisor log.Emitter interface.
func (gvisorEmitter) Emit(_ int, level gvlog.Level, _ time.Time, format string, v ...any) {
	libLogger.Load().Debug(fmt.Sprintf(format, v...), "lib", "gvisor", "level", level.String())
}
