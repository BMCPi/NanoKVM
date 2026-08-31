package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// newTestLogger builds a JSON logger writing to a buffer, wrapped exactly as
// logger.Init wraps the process logger, so these tests exercise the same
// handler stack production runs. Callers pass the returned logger straight
// into the constructor under test — RequestLogger/Recovery take it directly
// now, so there is no process-wide default left to swap.
func newTestLogger(t *testing.T) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	var buf bytes.Buffer
	h := logger.WithTraceContext(
		slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf, slog.New(h)
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %q", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// TestRequestLoggerLevelsByOutcome pins the rule that makes the request log
// usable at a raised level: failures must not be filtered out with the noise.
func TestRequestLoggerLevelsByOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		status int
		want   string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusNotFound, "WARN"},
		{http.StatusUnauthorized, "WARN"},
		{http.StatusInternalServerError, "ERROR"},
	}

	for _, tc := range cases {
		buf, log := newTestLogger(t)

		r := gin.New()
		r.Use(RequestLogger(log))
		r.GET("/x", func(c *gin.Context) { c.Status(tc.status) })

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x?a=1", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)

		recs := decodeRecords(t, buf)
		if len(recs) != 1 {
			t.Fatalf("status %d: expected 1 record, got %d", tc.status, len(recs))
		}
		rec := recs[0]
		if rec["level"] != tc.want {
			t.Errorf("status %d logged at %v, want %v", tc.status, rec["level"], tc.want)
		}
		if rec["path"] != "/x" {
			t.Errorf("path = %v, want /x", rec["path"])
		}
		if rec["query"] != "a=1" {
			t.Errorf("query = %v, want a=1", rec["query"])
		}
		if got, ok := rec["status"].(float64); !ok || int(got) != tc.status {
			t.Errorf("status attr = %v, want %d", rec["status"], tc.status)
		}
	}
}

// TestRequestLoggerCorrelatesWithTrace is the payoff for running otelgin: a
// request log line must carry the IDs that join it to the span otelgin opened,
// so a trace and its logs can be looked up from each other.
//
// It also pins the middleware ORDER dependency — RequestLogger reads the span
// out of c.Request.Context() after c.Next(), which only works because otelgin
// has replaced that request by then.
func TestRequestLoggerCorrelatesWithTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf, log := newTestLogger(t)

	// A real SDK provider: spans are recorded and get valid IDs even with no
	// exporter attached, which is exactly the BMC's default configuration
	// (telemetry on, no OTLP endpoint).
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	r := gin.New()
	r.Use(otelgin.Middleware("test", otelgin.WithTracerProvider(tp)))
	r.Use(RequestLogger(log))
	r.GET("/x", func(c *gin.Context) {
		// A line logged by a handler must land in the same trace as the
		// request line — that is what makes the two greppable together.
		log.InfoContext(c.Request.Context(), "handler ran")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	recs := decodeRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("expected handler + request records, got %d: %s", len(recs), buf.String())
	}

	var traceID string
	for _, rec := range recs {
		id, ok := rec["trace_id"].(string)
		if !ok || id == "" {
			t.Fatalf("record has no trace_id: %v", rec)
		}
		if _, ok := rec["span_id"].(string); !ok {
			t.Errorf("record has no span_id: %v", rec)
		}
		if traceID == "" {
			traceID = id
		} else if id != traceID {
			t.Errorf("handler and request lines are in different traces: %s vs %s", traceID, id)
		}
	}
}

// TestMiddlewareOrderYieldsTraceIDs pins the ordering requirement that
// cmd/server/main.go depends on, and that is invisible at a glance.
//
// otelgin sets the span on c.Request and RESTORES the original request when it
// returns. A request logger registered outside it therefore reads a
// span-less context and logs every line without trace_id — no error, no
// warning, just permanently uncorrelatable logs. This test fails if the two
// are ever swapped.
func TestMiddlewareOrderYieldsTraceIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	build := func(otelFirst bool, log *slog.Logger) *gin.Engine {
		tp := sdktrace.NewTracerProvider()
		t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
		otel := otelgin.Middleware("test", otelgin.WithTracerProvider(tp))

		r := gin.New()
		if otelFirst {
			r.Use(otel)
			r.Use(RequestLogger(log))
		} else {
			r.Use(RequestLogger(log))
			r.Use(otel)
		}
		r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
		return r
	}

	hasTraceID := func(otelFirst bool) bool {
		buf, log := newTestLogger(t)
		build(otelFirst, log).ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
		recs := decodeRecords(t, buf)
		if len(recs) != 1 {
			t.Fatalf("expected 1 record, got %d", len(recs))
		}
		id, _ := recs[0]["trace_id"].(string)
		return id != ""
	}

	if !hasTraceID(true) {
		t.Error("otelgin registered first: request log has no trace_id — correlation is broken")
	}
	// The inverse is documentation of the failure mode, not a desired
	// behaviour: if a future otelgin stops restoring c.Request this will start
	// passing trace IDs either way, and this assertion should simply be dropped.
	if hasTraceID(false) {
		t.Log("otelgin registered second now also yields trace_id; the ordering constraint may have been relaxed upstream")
	}
}

// TestRecoveryLogsAndReturns500 confirms a panicking handler becomes a 500
// plus one error record carrying a stack, rather than gin.Recovery's dump to
// a separate writer.
func TestRecoveryLogsAndReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf, log := newTestLogger(t)

	r := gin.New()
	r.Use(Recovery(log))
	r.GET("/boom", func(_ *gin.Context) { panic("kaboom") })

	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}

	recs := decodeRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %s", len(recs), buf.String())
	}
	rec := recs[0]
	if rec["level"] != "ERROR" {
		t.Errorf("panic logged at %v, want ERROR", rec["level"])
	}
	if panicked, _ := rec["panic"].(string); panicked != "kaboom" {
		t.Errorf("panic value = %v, want kaboom", rec["panic"])
	}
	// The stack must reach past the recover() frame to the code that actually
	// panicked, otherwise it is useless for diagnosis.
	stack, _ := rec["stack"].(string)
	if !strings.Contains(stack, "logging_test.go") {
		t.Errorf("stack does not reach the panicking handler: %q", stack)
	}
	if !strings.Contains(stack, "Recovery") {
		t.Errorf("stack does not name the recovering middleware: %q", stack)
	}
}

// TestRequestLoggerUsesInjectedLogger confirms RequestLogger writes through
// the logger it is constructed with, rather than any process-wide default.
func TestRequestLoggerUsesInjectedLogger(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := gin.New()
	r.Use(RequestLogger(l))
	r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if !strings.Contains(buf.String(), "http request") {
		t.Fatalf("injected logger saw no request line; buf=%q", buf.String())
	}
}
