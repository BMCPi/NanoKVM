package auth

import (
	"log/slog"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

type loginAttempt struct {
	failures   int
	lastFailed time.Time
	lockoutEnd time.Time
}

const (
	maxLoginAttemptsRecords = 3000
	cleanupInterval         = 6 * time.Hour
)

// startCleanupRoutine starts a background routine to clean up memory. Exits
// when s.rootCtx is cancelled, instead of running for the rest of the
// process's life -- see cleanupDone's doc comment for how a test observes
// that.
func (s *Service) startCleanupRoutine() {
	conf := config.GetInstance()
	if conf.Security.LoginLockoutDuration <= 0 {
		return
	}

	go func() {
		defer close(s.cleanupDone)
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.rootCtx.Done():
				return
			case <-ticker.C:
				s.loginMu.Lock()
				now := time.Now()
				for ip, attempt := range s.loginAttempts {
					// Cleanup rules: if it has been locked and the lockout time has passed,
					// or (although not locked) it has been 30 minutes since the last failure,
					// remove this record
					if (!attempt.lockoutEnd.IsZero() && now.After(attempt.lockoutEnd)) ||
						(attempt.lockoutEnd.IsZero() && now.Sub(attempt.lastFailed) > 30*time.Minute) {
						delete(s.loginAttempts, ip)
					}
				}
				s.loginMu.Unlock()
			}
		}
	}()
}

// CheckLoginAttempt checks if a login attempt is allowed based on brute-force protection rules.
// Returning true means the IP/System is locked out, and an error string and error code are returned.
func (s *Service) CheckLoginAttempt(clientIP string) (bool, int, string) {
	conf := config.GetInstance()
	if conf.Security.LoginLockoutDuration <= 0 {
		return false, 0, ""
	}

	s.cleanupOnce.Do(s.startCleanupRoutine)

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	if attempt, exists := s.loginAttempts[clientIP]; exists {
		if time.Now().Before(attempt.lockoutEnd) {
			s.log.Debug("login blocked: account locked due to too many failed attempts", slog.String("ip", clientIP), slog.Time("until", attempt.lockoutEnd))
			return true, -5, "Account locked due to too many failed attempts, please try again later"
		}

		// If lockout has elapsed, then we reset the failures and lockoutEnd.
		if !attempt.lockoutEnd.IsZero() {
			attempt.failures = 0
			attempt.lockoutEnd = time.Time{}
		}
	}

	return false, 0, ""
}

// RecordLoginFailure records a failed login attempt for the given IP address.
func (s *Service) RecordLoginFailure(clientIP string) (bool, int, string) {
	conf := config.GetInstance()
	if conf.Security.LoginLockoutDuration <= 0 {
		return false, 0, ""
	}

	s.cleanupOnce.Do(s.startCleanupRoutine)

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	attempt, exists := s.loginAttempts[clientIP]
	if !exists {
		// When the record pool is full, clear the records instead of global lockout to prevent DDoS
		if len(s.loginAttempts) >= maxLoginAttemptsRecords {
			s.log.Warn("login attempt records reached maximum limit, clearing records to prevent memory overflow")
			s.loginAttempts = make(map[string]*loginAttempt)
		}
		attempt = &loginAttempt{}
		s.loginAttempts[clientIP] = attempt
	}

	now := time.Now()
	// Failure time window: if it has been a long time since the last failure
	// (e.g., beyond the lockoutDuration window), reset the failure count
	if !attempt.lastFailed.IsZero() && now.Sub(attempt.lastFailed) > time.Duration(conf.Security.LoginLockoutDuration)*time.Second {
		attempt.failures = 0
	}

	attempt.failures++
	attempt.lastFailed = now

	// Reach the failure limit, lock out
	if attempt.failures >= conf.Security.LoginMaxFailures {
		attempt.lockoutEnd = now.Add(time.Duration(conf.Security.LoginLockoutDuration) * time.Second)
		s.log.Debug("login failures reached threshold, locking out", slog.String("ip", clientIP), slog.Time("until", attempt.lockoutEnd))
	}

	return false, 0, ""
}

// ClearLoginAttempt clears the failed login attempt record for an IP upon successful login.
func (s *Service) ClearLoginAttempt(clientIP string) {
	conf := config.GetInstance()
	if conf.Security.LoginLockoutDuration <= 0 {
		return
	}

	s.loginMu.Lock()
	defer s.loginMu.Unlock()

	delete(s.loginAttempts, clientIP)
}
