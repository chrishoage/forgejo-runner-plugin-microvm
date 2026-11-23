// SPDX-License-Identifier: GPL-3.0-or-later

package jobnet

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// The library's error lines the job log takes: a burst of libraryLogBurst,
// then libraryLogRate a second. A guest can make the library log a line
// per packet, and the job log has no cap.
const (
	libraryLogRate  = 1
	libraryLogBurst = 10
)

// routeLibraryLog routes the library's logger, logrus's standard one, into
// log: only its errors, within the rate above, and nothing to stderr, and
// returns its hook. It is process-wide; the job process runs one job
// network.
func routeLibraryLog(log *slog.Logger) *libraryLog {
	l := logrus.StandardLogger()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.ErrorLevel)
	h := &libraryLog{log: log, limit: rate.NewLimiter(libraryLogRate, libraryLogBurst)}
	hooks := logrus.LevelHooks{}
	hooks.Add(h)
	l.ReplaceHooks(hooks)
	return h
}

// libraryLog is the logrus hook that writes the library's lines to the job
// log.
type libraryLog struct {
	log     *slog.Logger
	limit   *rate.Limiter
	dropped atomic.Int64
	// closing is set once the job network closes its switch ports. The
	// switch then logs each port's read of its closed conn as an error,
	// which the hook drops: it is the job's normal teardown.
	closing atomic.Bool
}

func (h *libraryLog) Levels() []logrus.Level {
	return []logrus.Level{logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel}
}

func (h *libraryLog) Fire(e *logrus.Entry) error {
	if h.closing.Load() && strings.Contains(e.Message, net.ErrClosed.Error()) {
		return nil
	}
	if !h.limit.Allow() {
		h.dropped.Add(1)
		return nil
	}
	h.log.Error("job network: "+e.Message, "dropped_before", h.dropped.Swap(0))
	return nil
}
