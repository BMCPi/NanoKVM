package logger

import (
	"log/slog"
	"sync/atomic"
)

// Holder is an upgradeable, race-free slot for an injected component logger.
//
// It exists for package-level singletons that something may reach before the
// owning package's own Start (or equivalent) call has run: an HTTP handler
// calling straight into a package-level helper, a package's own unit test
// exercising a helper directly, or another already-started subsystem
// reaching a shared singleton before this package's Start does. A
// sync.Once-guarded var is the wrong tool there — it latches onto whichever
// caller touches it first, and if that caller loses the race with Start, the
// real, component-tagged logger Start was given is discarded forever.
//
// Holder never latches on an unset read: every Get before the first Set
// returns the process default, and a subsequent Set always takes effect for
// every Get after it, however many reads happened first. The zero value is
// ready to use.
type Holder struct {
	p atomic.Pointer[slog.Logger]
}

// Set stores l (or the process default, via Or, when l is nil) as the
// holder's logger, overwriting whatever was stored before. Safe to call more
// than once — e.g. a Restart that re-derives the logger — and concurrently
// with Get.
func (h *Holder) Set(l *slog.Logger) {
	h.p.Store(Or(l))
}

// Get returns the most recently Set logger, or the process default if Set
// has never been called. It never latches: an early Get before Start runs
// does not fix the holder's value, so a later Set from Start still takes
// effect for every subsequent Get.
func (h *Holder) Get() *slog.Logger {
	if l := h.p.Load(); l != nil {
		return l
	}
	return slog.Default()
}
