package logger

// otel.go correlates log records with traces.
//
// The server already runs otelgin, so every HTTP request gets a span. Without
// this wrapper a log line emitted during that request carries no reference to
// it, and the trace and the logs can never be joined — which is most of the
// value of having both.
//
// This is deliberately not the go.opentelemetry.io/contrib/bridges/otelslog
// bridge. That bridge EXPORTS log records through an OTel LoggerProvider,
// which means pulling in the OTel logs SDK and running a third exporter on a
// device with 128 MB of RAM. All that is wanted here is the correlation IDs on
// the lines already being written, which is what this does using the trace API
// that is linked in regardless.

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler decorates records that were logged with a span-carrying
// context, adding the IDs a log backend needs to link the line to its trace.
type traceHandler struct{ slog.Handler }

// WithTraceContext wraps h so records logged via the *Context variants
// (InfoContext, ErrorContext, …) gain trace_id and span_id. Records logged
// without a context, or with one that carries no recording span, pass through
// untouched — no empty attrs.
//
// Init applies this to the process logger. It is exported so tests that build
// their own handler get the same correlation behaviour as production rather
// than silently losing it.
func WithTraceContext(h slog.Handler) slog.Handler { return traceHandler{Handler: h} }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
