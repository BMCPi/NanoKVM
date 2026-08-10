// Package serial provides a shared serial terminal broker that allows
// multiple concurrent consumers (WebSocket, IPMI SOL, Redfish) to read
// from and write to the same serial port.
package serial

import (
	"io"
	"sync"
)

// MultiWriter fans out writes to a dynamic set of writers.
// Writers can be added and removed concurrently while writes are in progress.
// Inspired by github.com/alanshaw/multiwriter and tinkerbell/secondstar.
type MultiWriter struct {
	mu      sync.RWMutex
	writers map[io.Writer]struct{}
}

// NewMultiWriter creates a MultiWriter with an optional initial set of writers.
func NewMultiWriter(writers ...io.Writer) *MultiWriter {
	mw := &MultiWriter{writers: make(map[io.Writer]struct{}, len(writers))}
	for _, w := range writers {
		mw.writers[w] = struct{}{}
	}
	return mw
}

// Write sends p to every registered writer. If any writer returns an error,
// that writer is skipped (not removed) but the write continues to the
// remaining writers. This differs from io.MultiWriter which stops on first
// error — for a broadcast scenario we want best-effort delivery.
func (mw *MultiWriter) Write(p []byte) (int, error) {
	mw.mu.RLock()
	defer mw.mu.RUnlock()

	for w := range mw.writers {
		if _, err := w.Write(p); err != nil {
			// Best-effort: log or ignore. The session's Detach will
			// clean up writers that are permanently broken.
			continue
		}
	}
	return len(p), nil
}

// Add registers a writer to receive future writes.
func (mw *MultiWriter) Add(w io.Writer) {
	mw.mu.Lock()
	mw.writers[w] = struct{}{}
	mw.mu.Unlock()
}

// Remove unregisters a writer.
func (mw *MultiWriter) Remove(w io.Writer) {
	mw.mu.Lock()
	delete(mw.writers, w)
	mw.mu.Unlock()
}

// Len returns the current number of registered writers.
func (mw *MultiWriter) Len() int {
	mw.mu.RLock()
	defer mw.mu.RUnlock()
	return len(mw.writers)
}
