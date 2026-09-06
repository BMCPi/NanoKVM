// Package auth is the BMC's credential store and brute-force guard: account
// read/write (bcrypt-hashed, persisted to accountFile), password changes
// (the web account and the root shell, kept in sync), the Basic-Auth
// fast-path cache, and per-client lockout tracking shared by the web login
// form, the Redfish session/Basic-Auth paths, and the SSH server.
package auth

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// Service holds pkg/auth's process state: brute-force lockout tracking and
// the Basic-Auth fast-path cache. Both used to be package-level singletons —
// this meant every caller (web login, SSH, Redfish) shared one global map,
// which was the point for lockout (see pkg/ssh/server.go's authPassword) but
// made the two impossible to isolate in tests. Moving them onto Service keeps
// the sharing (callers pass around the one *Service main constructs) while
// letting a test build an independent instance.
type Service struct {
	log *slog.Logger

	// rootCtx is the process-lifetime context passed to NewService. It bounds
	// the root-password exec in password.go (a generous timeout, not this
	// ctx directly -- see ChangePassword) and is what the brute-force cleanup
	// goroutine in brute_force.go selects on to exit at shutdown instead of
	// running for the rest of the process's life.
	rootCtx context.Context

	loginMu       sync.Mutex
	loginAttempts map[string]*loginAttempt
	cleanupOnce   sync.Once
	// cleanupDone closes when the brute-force cleanup goroutine (started by
	// startCleanupRoutine) returns. Exists for tests to observe that
	// cancelling rootCtx actually stops the goroutine rather than leaking it.
	cleanupDone chan struct{}

	cache *authCache

	// bcryptMu serializes the expensive password comparison. bcrypt is
	// CPU-bound and this BMC's SoC has a single core, so concurrent
	// comparisons contend near-linearly rather than overlapping: measured on
	// the device, one bcrypt at DefaultCost costs ~3.2s and three concurrent
	// Redfish logins took ~6.5s each. gofish and bmclib both open several
	// sessions at once, so that is the ordinary case for this service.
	//
	// Serializing does not make an attacker's life easier: every distinct
	// guess still runs a full comparison (see ComparePlainAccount), it just
	// cannot thrash the one core. What it does buy is that N concurrent
	// checks of the SAME credential collapse to one, because the waiters
	// re-read the cache the winner populated.
	bcryptMu sync.Mutex
}

// NewService constructs a Service. auth is a library, not a component: log is
// used exactly as given, never tagged with an additional "component" —
// callers (api/auth, pkg/ssh) already carry their own component-tagged
// logger and pass it straight through.
//
// ctx is normally the process-lifetime context; tests that never trigger the
// cleanup routine or a bounded exec can pass context.Background().
func NewService(ctx context.Context, log *slog.Logger) *Service {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Service{
		rootCtx:       ctx,
		log:           logger.Or(log),
		loginAttempts: make(map[string]*loginAttempt),
		cleanupDone:   make(chan struct{}),
		cache:         newAuthCache(),
	}
}
