package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/trace"
	"gopkg.in/natefinch/lumberjack.v2"
)

// TestNewFileWriter verifies that configuring a file path yields a rotating
// writer that actually creates and appends to the target file.
func TestNewFileWriter(t *testing.T) {
	// A nested path exercises the directory creation.
	path := filepath.Join(t.TempDir(), "logs", "server.log")

	w, err := newFileWriter(path)
	if err != nil {
		t.Fatalf("newFileWriter: %v", err)
	}
	defer func() { _ = w.Close() }()

	if _, ok := any(w).(*lumberjack.Logger); !ok {
		t.Fatalf("expected a *lumberjack.Logger, got %T", w)
	}
	if w.MaxSize <= 0 || w.MaxBackups <= 0 {
		t.Fatalf("rotation not configured: MaxSize=%d MaxBackups=%d", w.MaxSize, w.MaxBackups)
	}

	const line = "hello file logging\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back log file: %v", err)
	}
	if !strings.Contains(string(got), "hello file logging") {
		t.Fatalf("log file missing written line, got %q", got)
	}
}

// TestNewFileWriterUnwritable confirms an unwritable path surfaces an error so
// Init can fall back to stdout rather than silently dropping logs.
func TestNewFileWriterUnwritable(t *testing.T) {
	// A file used as a directory component can't be created into.
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := newFileWriter(filepath.Join(blocker, "server.log")); err == nil {
		t.Fatal("expected error for unwritable path, got nil")
	}
}

// TestWriterReflectsConfiguredFile verifies the shared Writer() is pointed at
// the configured file after the switch selects the file branch, and that
// Close flushes it. It drives the same output-selection logic Init uses
// without depending on the global config singleton.
func TestWriterReflectsConfiguredFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")

	w, err := newFileWriter(path)
	if err != nil {
		t.Fatalf("newFileWriter: %v", err)
	}
	prevOut, prevCloser := output, closer
	t.Cleanup(func() {
		output, closer = prevOut, prevCloser
		_ = w.Close()
	})

	output, closer = w, w
	slog.New(slog.NewJSONHandler(output, nil)).Info("routed to file")

	if Writer() != w {
		t.Fatal("Writer() did not return the configured file writer")
	}
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back log file: %v", err)
	}
	if !strings.Contains(string(got), "routed to file") {
		t.Fatalf("slog output not written to file, got %q", got)
	}
}

// TestParseLevel pins the mapping from the config file's level names (which
// are logrus's) onto slog levels, including the names slog has no direct
// equivalent for. An existing /etc/kvm/server.yaml must keep working.
func TestParseLevel(t *testing.T) {
	cases := []struct {
		name string
		want slog.Level
	}{
		{"trace", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"  info  ", slog.LevelInfo}, // surrounding whitespace is trimmed
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"fatal", slog.LevelError},
		{"panic", slog.LevelError},
		// Unparseable falls back to errors-only, as the logrus setup did.
		{"nonsense", slog.LevelError},
		{"", slog.LevelError},
	}
	for _, tc := range cases {
		if got := parseLevel(tc.name); got != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConsoleHandlerFormat pins the console layout: the bracketed
// timestamp/level prefix the app has always printed, with structured attrs
// appended as key=value.
func TestConsoleHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	l := slog.New(newConsoleHandler(&buf, lv, false))

	l.With(slog.String("subsys", "power")).
		Warn("power-on requested", slog.String("op", "on"), slog.Int("attempt", 2))

	got := buf.String()
	for _, want := range []string{"[warning]", "power-on requested", "subsys=power", "op=on", "attempt=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("console line %q missing %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected exactly one line, got %q", got)
	}
}

// TestConsoleHandlerQuotesValuesWithSpaces keeps the key=value scan parseable
// when a value contains whitespace or an equals sign.
func TestConsoleHandlerQuotesValuesWithSpaces(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	l := slog.New(newConsoleHandler(&buf, lv, false))

	l.Info("msg", slog.String("path", "/var/log/a b.log"), slog.String("plain", "value"))

	got := buf.String()
	if !strings.Contains(got, `path="/var/log/a b.log"`) {
		t.Errorf("value with a space was not quoted: %q", got)
	}
	if !strings.Contains(got, "plain=value") {
		t.Errorf("value without a space should not be quoted: %q", got)
	}
}

// TestConsoleHandlerLevelFiltering confirms the handler honours the shared
// LevelVar, so SetLevel takes effect without rebuilding the logger.
func TestConsoleHandlerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	l := slog.New(newConsoleHandler(&buf, lv, false))

	l.Info("suppressed")
	if buf.Len() != 0 {
		t.Fatalf("info logged below threshold: %q", buf.String())
	}

	lv.Set(slog.LevelInfo)
	l.Info("emitted")
	if !strings.Contains(buf.String(), "emitted") {
		t.Fatalf("info not logged after lowering threshold: %q", buf.String())
	}
}

// TestLogrusBridge is the contract that lets the ~400 remaining logrus call
// sites stay put during the migration: a logrus call must land in the slog
// handler, at the mapped level, with its fields preserved as attrs — and must
// not also be written by logrus's own formatter.
func TestLogrusBridge(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)

	prev := level
	level = lv
	t.Cleanup(func() {
		level = prev
		logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
		logrus.SetOutput(os.Stderr)
		logrus.SetFormatter(&logrus.TextFormatter{})
		logrus.SetLevel(logrus.InfoLevel)
		logrus.SetReportCaller(false)
	})

	bridgeLogrus(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: lv})))

	logrus.WithField("name", "host.cap").Errorf("capsule staging failed: %v", os.ErrNotExist)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("bridged line is not JSON (%v): %q", err, buf.String())
	}
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
	if msg, _ := rec["msg"].(string); !strings.Contains(msg, "capsule staging failed") {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["name"] != "host.cap" {
		t.Errorf("logrus field lost: %v", rec)
	}
}

// TestLogrusBridgeRespectsLevel confirms level filtering happens on the slog
// side. The bridge sets logrus itself to TraceLevel so nothing is dropped
// before the hook runs; the handler is what must filter.
func TestLogrusBridgeRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)

	prev := level
	level = lv
	t.Cleanup(func() {
		level = prev
		logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
		logrus.SetOutput(os.Stderr)
		logrus.SetFormatter(&logrus.TextFormatter{})
		logrus.SetLevel(logrus.InfoLevel)
		logrus.SetReportCaller(false)
	})

	bridgeLogrus(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: lv})))

	logrus.Debug("noisy")
	if buf.Len() != 0 {
		t.Fatalf("debug line survived a warn-level handler: %q", buf.String())
	}

	logrus.Warn("audible")
	if !strings.Contains(buf.String(), "audible") {
		t.Fatalf("warn line dropped: %q", buf.String())
	}
}

// TestTraceHandlerNoSpanAddsNothing guards against the wrapper polluting every
// line with empty correlation IDs when nothing is traced — which is the normal
// case on a BMC with telemetry disabled.
func TestTraceHandlerNoSpanAddsNothing(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(WithTraceContext(slog.NewJSONHandler(&buf, nil)))

	l.InfoContext(context.Background(), "no trace here")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id added without a span: %v", rec)
	}
	if _, ok := rec["span_id"]; ok {
		t.Errorf("span_id added without a span: %v", rec)
	}
}

// TestTraceHandlerAddsSpanIDs is the correlation contract: a line logged
// during a traced request must carry the IDs that join it to that trace.
// Without this, otelgin's spans and the log file can never be correlated.
func TestTraceHandlerAddsSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(WithTraceContext(slog.NewJSONHandler(&buf, nil)))

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled},
	))

	l.ErrorContext(ctx, "power-on failed")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["trace_id"] != traceID.String() {
		t.Errorf("trace_id = %v, want %s", rec["trace_id"], traceID)
	}
	if rec["span_id"] != spanID.String() {
		t.Errorf("span_id = %v, want %s", rec["span_id"], spanID)
	}
}

// TestTraceHandlerPreservesWithAttrs confirms the wrapper does not drop
// logger-scoped attrs when it re-wraps the delegate handler.
func TestTraceHandlerPreservesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(WithTraceContext(slog.NewJSONHandler(&buf, nil))).
		With(slog.String("subsys", "firmware"))

	l.InfoContext(context.Background(), "staged")

	if !strings.Contains(buf.String(), `"subsys":"firmware"`) {
		t.Errorf("WithAttrs lost through the trace wrapper: %q", buf.String())
	}
}

// TestLogrusBridgeAttributesRealCaller pins call-site attribution for bridged
// lines.
//
// This is the case that defeats every PC-based approach: logrus.Debugf is
// inlined into its caller, so slog's AddSource and logrus's own
// SetReportCaller both resolve the shared PC to logrus/exported.go and point
// the reader at the wrong file. The bridge walks the stack and reports the
// site as a plain attr, which is why this passes.
func TestLogrusBridgeAttributesRealCaller(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)

	prev := level
	level = lv
	t.Cleanup(func() {
		level = prev
		logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
		logrus.SetOutput(os.Stderr)
		logrus.SetFormatter(&logrus.TextFormatter{})
		logrus.SetLevel(logrus.InfoLevel)
	})

	bridgeLogrus(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:     lv,
		AddSource: true,
	})))

	// Both the package-level function and an entry-based call must resolve to
	// THIS file, not to logrus internals.
	logrus.Debugf("package-level call")
	logrus.WithField("k", "v").Error("entry-based call")

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec struct {
			Msg    string `json:"msg"`
			Caller string `json:"caller"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if !strings.HasPrefix(rec.Caller, "logger_test.go:") {
			t.Errorf("%q attributed to %q, want logger_test.go:N", rec.Msg, rec.Caller)
		}
	}
}
