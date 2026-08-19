package logger

// formatter.go implements the console slog handler.
//
// It exists instead of slog.NewTextHandler because the console here is a
// serial line at 115200 baud with a human reading it, and logfmt's
// `time=2026-08-19T12:34:56.789Z level=INFO msg="..."` spends most of the
// terminal width on key names. This keeps the compact bracketed layout the
// app has always printed:
//
//	[2026-08-19 12:34:56.789] [INFO] [power.go:213] power-on requested op=on
//
// Structured attributes are appended after the message, so nothing is lost
// relative to the JSON file handler — it is only laid out for eyes.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const timeLayout = "2006-01-02 15:04:05.000"

type consoleHandler struct {
	w         io.Writer
	level     slog.Leveler
	addSource bool

	// preformatted holds attrs supplied via WithAttrs, already rendered, so
	// the per-record path does not re-render a logger's fixed fields.
	preformatted string
	// groups is the open group prefix applied to attr keys (WithGroup).
	groups []string

	mu *sync.Mutex
}

func newConsoleHandler(w io.Writer, level slog.Leveler, addSource bool) slog.Handler {
	return &consoleHandler{w: w, level: level, addSource: addSource, mu: &sync.Mutex{}}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := h.clone()
	var b strings.Builder
	b.WriteString(c.preformatted)
	for _, a := range attrs {
		appendAttr(&b, c.groups, a)
	}
	c.preformatted = b.String()
	return c
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.groups = append(append([]string(nil), c.groups...), name)
	return c
}

func (h *consoleHandler) clone() *consoleHandler {
	return &consoleHandler{
		w:            h.w,
		level:        h.level,
		addSource:    h.addSource,
		preformatted: h.preformatted,
		groups:       h.groups,
		mu:           h.mu,
	}
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	b.WriteByte('[')
	b.WriteString(r.Time.Format(timeLayout))
	b.WriteString("] [")
	b.WriteString(levelName(r.Level))
	b.WriteByte(']')

	if h.addSource && r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		if f, _ := fs.Next(); f.File != "" {
			b.WriteString(" [")
			b.WriteString(filepath.Base(f.File))
			b.WriteByte(':')
			b.WriteString(strconv.Itoa(f.Line))
			b.WriteByte(']')
		}
	}

	b.WriteByte(' ')
	b.WriteString(r.Message)
	b.WriteString(h.preformatted)

	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

// levelName renders the level the way logrus did (lower case, and "warning"
// rather than "WARN") so existing log greps and the operator's muscle memory
// keep working.
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warning"
	default:
		return "error"
	}
}

// appendAttr renders one attr as ` key=value`, flattening groups into dotted
// keys. Empty attrs are dropped, matching slog's documented handler contract.
func appendAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}

	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		sub := groups
		if a.Key != "" {
			sub = append(append([]string(nil), groups...), a.Key)
		}
		for _, nested := range attrs {
			appendAttr(b, sub, nested)
		}
		return
	}

	b.WriteByte(' ')
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')

	v := a.Value.String()
	// Quote only when the value would otherwise break the key=value scan.
	if v == "" || strings.ContainsAny(v, " \t\"=") {
		b.WriteString(strconv.Quote(v))
		return
	}
	b.WriteString(v)
}
