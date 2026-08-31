// Package auth is the BMC's credential store and brute-force guard: account
// read/write (bcrypt-hashed, persisted to accountFile), password changes
// (the web account and the root shell, kept in sync), the Basic-Auth
// fast-path cache, and per-client lockout tracking shared by the web login
// form, the Redfish session/Basic-Auth paths, and the SSH server.
package auth

import (
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

	loginMu       sync.Mutex
	loginAttempts map[string]*loginAttempt
	cleanupOnce   sync.Once

	cache *authCache
}

// NewService constructs a Service. auth is a library, not a component: log is
// used exactly as given, never tagged with an additional "component" —
// callers (api/auth, pkg/ssh) already carry their own component-tagged
// logger and pass it straight through.
func NewService(log *slog.Logger) *Service {
	return &Service{
		log:           logger.Or(log),
		loginAttempts: make(map[string]*loginAttempt),
		cache:         newAuthCache(),
	}
}
