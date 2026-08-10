package logger

import (
	"io"
	stdlog "log"
	"os"
	"path/filepath"

	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

// output is the shared destination for every logging path: logrus, the standard
// library logger, and — wired by the caller — gin's request logger. It is the
// rotating file writer when file logging is configured, otherwise os.Stdout.
var output io.Writer = os.Stdout

// closer holds the rotating file writer so Close can flush/release it on
// shutdown. Nil when logging to stdout.
var closer io.Closer

// Writer returns the active log destination so other subsystems (e.g. gin's
// request logger) can share the same rotating file instead of writing to a
// separate stdout stream.
func Writer() io.Writer { return output }

func Init() {
	conf := config.GetInstance()

	level, err := logrus.ParseLevel(conf.Logger.Level)
	if err != nil {
		level = logrus.ErrorLevel
	}
	logrus.SetLevel(level)

	file := conf.Logger.File
	switch file {
	case "", "console", "stdout":
		output = os.Stdout
	default:
		w, err := newFileWriter(file)
		if err != nil {
			logrus.Error("open log file failed:", err)
			output = os.Stdout
		} else {
			output = w
			closer = w
		}
	}

	logrus.SetOutput(output)
	logrus.SetReportCaller(true)
	logrus.SetFormatter(&formatter{})

	// Route the standard library logger (used for early-boot messages here and
	// by a few dependencies) into the same destination so nothing bypasses the
	// configured log file.
	stdlog.SetOutput(output)

	logrus.Info("logger set success")
}

// Close flushes and releases the rotating file writer. Safe to call when
// logging to stdout (no-op).
func Close() error {
	if closer != nil {
		return closer.Close()
	}
	return nil
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
