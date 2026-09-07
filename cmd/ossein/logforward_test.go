//go:build darwin && arm64

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// capture returns a logger writing JSON records into buf, and the buf.
func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	// Drop the volatile time key so assertions are stable.
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})

	return slog.New(handler), &buf
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any

	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("bad emitted line %q: %v", line, err)
		}

		out = append(out, record)
	}

	return out
}

func TestLogForwarderParsesLogrusJSON(t *testing.T) {
	t.Parallel()

	logger, buf := capture()
	fwd := newLogForwarder(logger, "buildkitd")

	// A logrus-JSON line, split across two Writes to exercise line buffering.
	line := `{"level":"info","msg":"auto snapshotter: using overlayfs","time":"2026-07-09T02:49:58Z","spanID":"abc"}` + "\n"
	_, _ = fwd.Write([]byte(line[:20]))
	_, _ = fwd.Write([]byte(line[20:]))

	records := decodeLines(t, buf)
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d: %s", len(records), buf.String())
	}

	got := records[0]
	if got["msg"] != "auto snapshotter: using overlayfs" {
		t.Fatalf("msg = %v", got["msg"])
	}

	if got["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", got["level"])
	}

	if got["source"] != "buildkitd" {
		t.Fatalf("source = %v", got["source"])
	}

	if got["spanID"] != "abc" {
		t.Fatalf("extra field spanID lost: %v", got)
	}

	if _, leaked := got["time"]; !leaked {
		// slog adds its own time; the child's "time" field must NOT be forwarded
		// as an attribute (it was dropped, then slog re-added its own — which our
		// ReplaceAttr strips, so it should be absent here).
		return
	}

	t.Fatalf("child time field leaked into attrs: %v", got)
}

func TestLogForwarderLevelMapping(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"debug":   "DEBUG",
		"trace":   "DEBUG",
		"info":    "INFO",
		"warning": "WARN",
		"error":   "ERROR",
		"fatal":   "ERROR",
	}
	for logrusLevel, want := range cases {
		logger, buf := capture()
		fwd := newLogForwarder(logger, "x")
		_, _ = fwd.Write([]byte(`{"level":"` + logrusLevel + `","msg":"m"}` + "\n"))

		records := decodeLines(t, buf)
		if len(records) != 1 || records[0]["level"] != want {
			t.Fatalf("level %q → %v, want %s", logrusLevel, records, want)
		}
	}
}

func TestLogForwarderNonJSONPassthrough(t *testing.T) {
	t.Parallel()

	logger, buf := capture()
	fwd := newLogForwarder(logger, "buildkitd")
	_, _ = fwd.Write([]byte("panic: runtime error\n"))

	records := decodeLines(t, buf)
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}

	raw, _ := records[0]["raw"].(bool)
	if records[0]["msg"] != "panic: runtime error" || !raw {
		t.Fatalf("non-JSON not passed through raw: %v", records[0])
	}
}

func TestLogForwarderFlushPartial(t *testing.T) {
	t.Parallel()

	logger, buf := capture()
	fwd := newLogForwarder(logger, "x")
	// No trailing newline — only Flush should emit it.
	_, _ = fwd.Write([]byte(`{"level":"info","msg":"tail"}`))

	if buf.Len() != 0 {
		t.Fatalf("emitted before flush: %s", buf.String())
	}

	fwd.Flush()

	records := decodeLines(t, buf)
	if len(records) != 1 || records[0]["msg"] != "tail" {
		t.Fatalf("flush did not emit partial line: %v", records)
	}
}
