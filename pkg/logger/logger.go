// Package logger owns every logging path in the server: the slog default
// logger, the destination (stdout or a rotating file), the OpenTelemetry
// trace correlation, and the bridge that keeps the codebase's remaining
// logrus call sites flowing into the same handler.
//
// slog is the only logger the app configures. The standard library's `log`
// package is redirected into it by slog.SetDefault, and logrus is redirected
// into it by the hook in logrus_bridge.go, so a line emitted through any of
// the three ends up in one destination, in one format, at one level.
//
// Format depends on the destination, because the two are read by different
// readers. Console output is the serial console at 115200 baud, where a human
// is looking for one line, so it stays in the compact bracketed form the app
// has always used. File output is collected and grepped by machines, so it is
// JSON.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"gopkg.in/natefinch/lumberjack.v2"
)

// output is the shared destination for every logging path: slog, the standard
// library logger, logrus (via the bridge), and — wired by the caller — gin's
// error writer. It is the rotating file writer when file logging is
// configured, otherwise os.Stdout.
var output io.Writer = os.Stdout

// closer holds the rotating file writer so Close can flush/release it on
// shutdown. Nil when logging to stdout.
var closer io.Closer

// level is the configured minimum level. A LevelVar rather than a plain Level
// so the provisional handler installed at the top of Init and the real one
// built below share one threshold, and setting it once from config updates
// both.
var level = new(slog.LevelVar)

// Writer returns the active log destination so other subsystems (e.g. gin's
// error writer, which bypasses slog entirely) can share the same rotating
// file instead of writing to a separate stdout stream.
func Writer() io.Writer { return output }

// Init configures the process-wide logger from config and installs it as
// slog's default. It returns the logger for callers that would rather hold a
// reference than go through slog's package-level functions.
//
// Init must run before any subsystem starts: until it does, slog's default
// writes unleveled text to stderr, which on this device is nowhere useful.
func Init() *slog.Logger {
	// Bridge both loggers to a provisional stdout handler BEFORE reading
	// config. Loading config emits its own messages through logrus (the
	// hardware probe) and through the standard library (defaults applied, JWT
	// secret persisted, "authentication is disabled"), and reading it first
	// would let those escape in two foreign formats before anything is
	// configured. The level var is Info until config overrides it below, which
	// is the right default for boot messages.
	provisional := slog.New(WithTraceContext(newConsoleHandler(os.Stdout, level, false)))
	slog.SetDefault(provisional)
	bridgeLogrus(provisional)

	conf := config.GetInstance()

	level.Set(parseLevel(conf.Logger.Level))

	// AddSource costs a runtime.CallersFrames walk per emitted record. That is
	// worth paying while debugging on a device this small, and not otherwise.
	addSource := level.Level() <= slog.LevelDebug

	var (
		handler slog.Handler
		dest    string
	)
	switch file := conf.Logger.File; file {
	case "", "console", "stdout":
		output, dest = os.Stdout, "stdout"
		handler = newConsoleHandler(output, level, addSource)
	default:
		w, err := newFileWriter(file)
		if err != nil {
			// Report against the provisional logger installed above, then
			// carry on to stdout — a BMC that cannot open its log file still
			// has to boot.
			output, dest = os.Stdout, "stdout"
			handler = newConsoleHandler(output, level, addSource)
			provisional.Error("logger: cannot open log file, falling back to stdout",
				slog.String("path", file), slog.Any("err", err))
		} else {
			output, closer, dest = w, w, file
			handler = slog.NewJSONHandler(output, &slog.HandlerOptions{
				Level:     level,
				AddSource: addSource,
			})
		}
	}

	// Attach trace/span IDs to any record logged with a context carrying a
	// span, so a log line during a traced request joins that trace instead of
	// being an isolated data point.
	handler = WithTraceContext(handler)

	l := slog.New(handler)
	slog.SetDefault(l)

	// slog.SetDefault also repoints the standard library's log package at this
	// handler, which covers the early-boot log.Printf calls in main and config
	// plus anything a dependency logs. Do NOT call log.SetOutput after this:
	// it would replace that bridge with a raw writer and lose the level and
	// format.

	// Route the codebase's remaining logrus call sites here too.
	bridgeLogrus(l)

	l.Info("logger initialized",
		slog.String("level", level.Level().String()),
		slog.String("destination", dest))
	return l
}

// Close flushes and releases the rotating file writer. Safe to call when
// logging to stdout (no-op).
func Close() error {
	if closer != nil {
		return closer.Close()
	}
	return nil
}

// SetLevel changes the process-wide minimum level at runtime. It is strict
// where parseLevel is lenient: an unrecognised name is an error and leaves
// the level unchanged, because the caller (the settings UI) shows the name to
// the operator, and silently logging at a different level than the one
// displayed is worse than refusing.
//
// The shared LevelVar means the change applies to every path at once: native
// slog call sites, the standard library bridge, and the logrus bridge, whose
// hook filters against this same threshold.
func SetLevel(name string) error {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	switch trimmed {
	case "trace", "debug", "info", "warn", "warning", "error", "fatal", "panic":
		level.Set(parseLevel(trimmed))
		return nil
	default:
		return fmt.Errorf("logger: unrecognised level %q", name)
	}
}

// Or returns l, or the process default logger when l is nil. It is the
// standard nil-guard for injected loggers: constructors accept a logger and
// call Or once, so hand-built test fixtures that leave the field nil keep
// working instead of panicking.
func Or(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// parseLevel maps the configured level name onto a slog level. It accepts the
// logrus names the config file has always used (including "warning" and
// "fatal", which slog has no direct equivalent for) so an existing
// /etc/kvm/server.yaml keeps working unchanged.
func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "trace", "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		// Matches the previous behaviour: an unparseable level logs errors only.
		return slog.LevelError
	}
}

// newFileWriter returns a size-rotating writer for path. Rotation is essential
// on this device: the log lives on the fixed-size rootfs, so an unbounded file
// would eventually fill it and the daemon owns the file for the process's whole
// lifetime.
func newFileWriter(path string) (*lumberjack.Logger, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return nil, err
	}

	// Verify the file is writable now so we can fall back to stdout on failure,
	// instead of only discovering the problem on the first log write (lumberjack
	// opens the file lazily).
	fh, err := os.OpenFile(absPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	_ = fh.Close()

	return &lumberjack.Logger{
		Filename:   absPath,
		MaxSize:    10, // megabytes before the file is rotated
		MaxBackups: 3,  // retain at most 3 rotated files
		MaxAge:     28, // days to keep a rotated file
		Compress:   true,
	}, nil
}
