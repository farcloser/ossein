//go:build darwin && arm64

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// logForwarder is an io.Writer that parses a child's logrus-JSON log stream
// (buildkitd --log-format json) line by line and re-emits each record through
// our slog logger, so a subprocess's logs share the host's format, level, and
// destination. Non-JSON lines (pre-logrus startup, panics) pass through raw.
//
// It is safe for concurrent writers (buildkitd's stdout and stderr both feed
// one forwarder).
type logForwarder struct {
	logger *slog.Logger
	source string

	mu  sync.Mutex
	buf []byte
}

func newLogForwarder(logger *slog.Logger, source string) *logForwarder {
	return &logForwarder{logger: logger, source: source}
}

func (w *logForwarder) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, data...)

	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}

		w.emit(w.buf[:idx])
		w.buf = w.buf[idx+1:]
	}

	return len(data), nil
}

// Flush emits any buffered partial line (no trailing newline). Call after the
// child has exited.
func (w *logForwarder) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

func (w *logForwarder) emit(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}

	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		// Not JSON — a pre-logrus startup line or a panic trace. Keep it.
		w.logger.Info(string(line), "source", w.source, "raw", true)

		return
	}

	// Absent or non-string msg/level degrade to ""/info rather than dropping
	// the record.
	var msg, lvl string

	if value, ok := record["msg"].(string); ok {
		msg = value
	}

	if value, ok := record["level"].(string); ok {
		lvl = value
	}

	level := logrusLevel(lvl)

	attrs := []slog.Attr{slog.String("source", w.source)}

	for key, val := range record {
		switch key {
		case "msg", "level", "time": // dropped: slog supplies msg/level/time itself
			continue
		default:
			attrs = append(attrs, slog.Any(key, val))
		}
	}

	w.logger.LogAttrs(context.Background(), level, msg, attrs...)
}

// logrusLevel maps a logrus level string to a slog level.
func logrusLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug", "trace":
		return slog.LevelDebug
	case "warning", "warn":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
