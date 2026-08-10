package serial

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// captureSessionID is the broker session the always-on capture registers as.
// While it is connected the port stays open, so host serial output — most
// importantly the managed host's boot logs — is recorded even when no web
// terminal or SOL session is watching.
const captureSessionID = "capture"

// captureRetryInterval paces reconnect attempts when the serial device cannot
// be opened (missing node, transient open error).
const captureRetryInterval = 5 * time.Second

var (
	captureMu     sync.Mutex
	captureFile   *captureWriter
	captureCancel chan struct{}
)

// StartCapture begins the always-on capture of host serial output to the
// bounded file configured in serial.capture. It connects to the shared broker
// as a permanent session (retrying until the device opens) so the port runs
// for the whole server lifetime. No-op when disabled.
func StartCapture() {
	cfg := config.GetInstance().Serial.Capture
	if !cfg.Enabled {
		return
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCancel != nil {
		return // already running
	}
	captureFile = newCaptureWriter(cfg.File, int64(cfg.MaxSizeKB)*1024)
	cancel := make(chan struct{})
	captureCancel = cancel

	go func() {
		for {
			_, err := GetBroker().Connect(captureSessionID, captureFile)
			if err == nil {
				log.Infof("serial: capture to %s (max %d KB)", cfg.File, cfg.MaxSizeKB)
				return
			}
			log.Warnf("serial: capture: %v (retry in %s)", err, captureRetryInterval)
			select {
			case <-cancel:
				return
			case <-time.After(captureRetryInterval):
			}
		}
	}()
}

// StopCapture disconnects the capture session and closes the file. Idempotent.
func StopCapture() {
	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCancel == nil {
		return
	}
	close(captureCancel)
	captureCancel = nil
	GetBroker().Disconnect(captureSessionID)
	if captureFile != nil {
		captureFile.Close()
		captureFile = nil
	}
}

// CaptureFiles returns the capture file paths in chronological order (the
// rotated generation first), skipping ones that do not exist. Used by the API
// download handler.
func CaptureFiles() []string {
	cfg := config.GetInstance().Serial.Capture
	if cfg.File == "" {
		return nil
	}
	var out []string
	for _, p := range []string{cfg.File + ".1", cfg.File} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

// captureWriter appends to a size-capped file, rotating once (file → file.1)
// on overflow — so at most 2×max bytes are retained and a chatty console can
// never fill the data partition. No fsync per write: the payload is a
// console log; losing the tail on power cut is acceptable, SD wear is not.
type captureWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	size int64
	f    *os.File
}

func newCaptureWriter(path string, maxBytes int64) *captureWriter {
	return &captureWriter{path: path, max: maxBytes}
}

// Write implements io.Writer for the broker fan-out. Errors are swallowed
// after logging: a capture failure must never disturb the live sessions
// sharing the MultiWriter.
func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		if err := w.openLocked(); err != nil {
			log.Debugf("serial: capture open: %v", err)
			return len(p), nil
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		log.Debugf("serial: capture write: %v", err)
		_ = w.f.Close()
		w.f = nil
		return len(p), nil
	}
	if w.max > 0 && w.size >= w.max {
		w.rotateLocked()
	}
	return len(p), nil
}

func (w *captureWriter) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f = f
	w.size = st.Size()
	return nil
}

func (w *captureWriter) rotateLocked() {
	_ = w.f.Close()
	w.f = nil
	w.size = 0
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		log.Debugf("serial: capture rotate: %v", err)
	}
}

// Close flushes and releases the capture file.
func (w *captureWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}
