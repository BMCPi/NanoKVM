package logger

// logrus_bridge.go keeps the codebase's existing logrus call sites working
// while they are migrated to slog.
//
// The app has ~400 `log.Errorf(...)` calls against `log
// "github.com/sirupsen/logrus"`. Converting them all in one change would be a
// large unreviewable diff, and leaving logrus configured independently would
// mean two formats, two levels and two destinations in the same file. So
// logrus keeps its API and loses its backend: a hook forwards every entry into
// slog, and logrus's own writer is pointed at io.Discard.
//
// Removing this file is the last step of the migration, not the first — once
// no package imports logrus, bridgeLogrus becomes a no-op and can go.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// bridgeLogrus routes logrus through l and silences logrus's own output.
func bridgeLogrus(l *slog.Logger) {
	logrus.SetOutput(io.Discard)
	logrus.SetFormatter(&discardFormatter{})
	// The hook decides what is emitted, so logrus itself must not filter first.
	// Level filtering happens in the slog handler.
	logrus.SetLevel(logrus.TraceLevel)
	// Not logrus.SetReportCaller: its caller detection anchors on the first
	// logrus frame it ever sees and mis-attributes package-level calls
	// (log.Debugf) to logrus's own exported.go. The hook walks the stack
	// itself instead — see callerPC — which is both correct and one less
	// thing for logrus to compute.
	logrus.SetReportCaller(false)
	logrus.StandardLogger().ReplaceHooks(logrus.LevelHooks{})
	logrus.AddHook(&slogHook{logger: l})
}

// slogHook forwards logrus entries into slog.
type slogHook struct{ logger *slog.Logger }

func (h *slogHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *slogHook) Fire(e *logrus.Entry) error {
	lvl := toSlogLevel(e.Level)
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if !h.logger.Enabled(ctx, lvl) {
		return nil
	}

	// PC is deliberately 0: see callerSite. The call site travels as an
	// explicit attr instead, so it is never silently wrong.
	r := slog.NewRecord(e.Time, lvl, e.Message, 0)
	if site := callerSite(); site != "" {
		r.AddAttrs(slog.String("caller", site))
	}
	for k, v := range e.Data {
		r.AddAttrs(slog.Any(k, v))
	}
	// logrus.Fatal exits the process after hooks run, and logrus.Panic panics;
	// both are preserved because logrus, not this hook, drives them.
	return h.logger.Handler().Handle(ctx, r)
}

// callerSite returns "file.go:123" for the code that called logrus, or "" if
// no such frame is found.
//
// It reports a string rather than setting the record's PC, because a PC cannot
// express this. logrus's package-level helpers (logrus.Debugf and friends) are
// thin enough that the compiler inlines them into their caller, so ONE physical
// PC expands to two logical frames — logrus.Debugf, then the real caller — and
// anything that later resolves that PC (slog's AddSource, logrus's own
// getCaller) takes the first and reports exported.go. The frame walk here can
// see past that; a PC handed downstream cannot.
//
// So bridged records carry "caller" while native slog records carry "source".
// The difference is a marker of what has not been migrated yet, and it goes
// away with this file.
func callerSite() string {
	var pcs [16]uintptr
	// Skip runtime.Callers and callerSite itself; the rest of the walk filters.
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.File != "" && !isLoggingFrame(f.File) {
			return filepath.Base(f.File) + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return ""
		}
	}
}

// isLoggingFrame reports whether file belongs to logrus or to this bridge —
// the frames between the caller and the slog handler.
func isLoggingFrame(file string) bool {
	return strings.Contains(file, "/sirupsen/logrus") ||
		strings.HasSuffix(file, "pkg/logger/logrus_bridge.go")
}

func toSlogLevel(l logrus.Level) slog.Level {
	switch l {
	case logrus.TraceLevel, logrus.DebugLevel:
		return slog.LevelDebug
	case logrus.InfoLevel:
		return slog.LevelInfo
	case logrus.WarnLevel:
		return slog.LevelWarn
	case logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// discardFormatter satisfies logrus's formatter interface without allocating a
// rendered line, since the output is io.Discard anyway.
type discardFormatter struct{}

func (*discardFormatter) Format(*logrus.Entry) ([]byte, error) { return nil, nil }
