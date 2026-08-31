package middleware

// logging.go replaces gin.Logger() and gin.Recovery() with slog-backed
// equivalents.
//
// gin's own middleware writes its own text layout to its own writer. Pointing
// that writer at the app's log file put a third format in there alongside
// slog's and the standard library's, none of which a collector could parse as
// one stream. These emit the same information as slog records, so a request
// line carries the same level, the same destination, and — because they log
// with the request context — the same trace_id as everything the handler
// logged while serving it.

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// RequestLogger logs one record per served request.
//
// The level reflects the outcome so an operator can raise the log level and
// still see failures: 5xx logs at error, 4xx at warn, everything else at info.
func RequestLogger(log *slog.Logger) gin.HandlerFunc {
	log = logger.Or(log)
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Duration("latency", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
		}
		if raw != "" {
			attrs = append(attrs, slog.String("query", raw))
		}
		if n := c.Writer.Size(); n > 0 {
			attrs = append(attrs, slog.Int("bytes", n))
		}
		if errs := c.Errors.ByType(gin.ErrorTypePrivate).String(); errs != "" {
			attrs = append(attrs, slog.String("errors", strings.TrimSpace(errs)))
		}

		// Log against the request context so the record picks up the otelgin
		// span and lands in the same trace as the handler's own lines.
		ctx := c.Request.Context()
		switch {
		case status >= http.StatusInternalServerError:
			log.ErrorContext(ctx, "http request", attrs...)
		case status >= http.StatusBadRequest:
			log.WarnContext(ctx, "http request", attrs...)
		default:
			log.InfoContext(ctx, "http request", attrs...)
		}
	}
}

// Recovery turns a panicking handler into a 500 and an error record with a
// stack, instead of gin.Recovery()'s stderr dump.
//
// A broken pipe is handled separately: the client hung up mid-response, the
// connection is already gone, and writing a status to it would panic again. It
// is a client event, not a server fault, so it must not be logged at error —
// on a BMC, browsers abandon the video and SSE streams constantly.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	log = logger.Or(log)
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}

			ctx := c.Request.Context()
			if isBrokenPipe(r) {
				log.WarnContext(ctx, "http connection closed by peer",
					slog.String("path", c.Request.URL.Path),
					slog.Any("err", r))
				// The response is unwritable; abort without touching it.
				c.Abort()
				return
			}

			buf := make([]byte, 8<<10)
			buf = buf[:runtime.Stack(buf, false)]
			log.ErrorContext(ctx, "panic recovered in http handler",
				slog.String("method", c.Request.Method),
				slog.String("path", c.Request.URL.Path),
				slog.Any("panic", r),
				slog.String("stack", string(buf)))

			c.AbortWithStatus(http.StatusInternalServerError)
		}()

		c.Next()
	}
}

func isBrokenPipe(r any) bool {
	ne, ok := r.(error)
	if !ok {
		return false
	}
	var opErr *net.OpError
	if !errors.As(ne, &opErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(opErr.Err, &sysErr) {
		return false
	}
	msg := strings.ToLower(sysErr.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}
