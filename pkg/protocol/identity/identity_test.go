package identity

// identity_test.go pins the one property that makes a discovered BMC the
// same BMC tomorrow. An inventory keyed on a UUID that changes per boot
// registers a new node every scan.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestBMCUUIDIsStableAcrossCalls(t *testing.T) {
	first := BMCUUID()
	if first != BMCUUID() {
		t.Error("BMCUUID changed between calls; inventory tools key on it")
	}
}

func TestBMCUUIDIsWellFormedOrEmpty(t *testing.T) {
	got := BMCUUID()
	if got == "" {
		t.Skip("no stable seed on this host; empty is the documented answer")
	}
	if len(got) != 36 {
		t.Errorf("BMCUUID() = %q, want a 36-char UUID", got)
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Fatalf("BMCUUID() = %q, not a UUID: %v", got, err)
	}
}

// The seed must come from something that outlives the process. Deriving it
// from the same inputs must reproduce the same UUID.
func TestBMCIdentitySeedIsDeterministic(t *testing.T) {
	seed := bmcIdentitySeed()
	if seed == "" {
		t.Skip("no stable identity source on this host")
	}
	if !strings.HasPrefix(seed, "machine-id:") && !strings.HasPrefix(seed, "mac:") {
		t.Errorf("seed %q is from neither machine-id nor a MAC", seed)
	}
	if again := bmcIdentitySeed(); again != seed {
		t.Errorf("seed is not deterministic: %q then %q", seed, again)
	}
}
