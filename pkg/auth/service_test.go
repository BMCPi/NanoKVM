package auth

import (
	"log/slog"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// TestServicesIsolateBruteForceState locks in the singleton→instance move:
// two Services must not share lockout state. Before this task, loginAttempts
// was a package-level map, so a second Service (or a second test) would have
// inherited whatever the first had already recorded.
func TestServicesIsolateBruteForceState(t *testing.T) {
	conf := config.GetInstance()
	saved := conf.Security
	conf.Security.LoginLockoutDuration = 60
	conf.Security.LoginMaxFailures = 5
	t.Cleanup(func() { conf.Security = saved })

	a := NewService(slog.New(slog.DiscardHandler))
	b := NewService(slog.New(slog.DiscardHandler))

	for range 10 {
		a.RecordLoginFailure("10.0.0.1")
	}
	if locked, _, _ := a.CheckLoginAttempt("10.0.0.1"); !locked {
		t.Fatal("a should be locked out after 10 failures")
	}
	if locked, _, _ := b.CheckLoginAttempt("10.0.0.1"); locked {
		t.Fatal("b must not share a's lockout state")
	}
}
