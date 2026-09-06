package auth

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TestComparePlainAccountCollapsesConcurrentIdenticalChecks pins the property
// that makes Redfish session storms survivable on this hardware.
//
// bcrypt is CPU-bound and the BMC's SoC has a single core, so concurrent
// comparisons contend near-linearly: measured on the device, one bcrypt at
// DefaultCost costs ~3.2s, and three concurrent logins each took ~6.5s.
// gofish and bmclib both open several sessions at once, so this is the normal
// case, not an edge case.
//
// Only the first of N concurrent checks of the SAME credential may run the
// expensive comparison; the rest must observe the cache the first one
// populates. Distinct credentials are deliberately NOT deduplicated -- each
// wrong guess must still pay full cost -- which TestBcryptGateStillChargesEachDistinctGuess covers.
func TestComparePlainAccountCollapsesConcurrentIdenticalChecks(t *testing.T) {
	s := NewService(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	account, err := s.GetAccount()
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	var calls atomic.Int32
	restore := compareHash
	compareHash = func(_, _ []byte) error {
		calls.Add(1)
		// Stand in for the real ~3.2s cost, long enough that every goroutine
		// is inside ComparePlainAccount before the first one finishes.
		time.Sleep(80 * time.Millisecond)
		return nil // treat the credential as correct
	}
	t.Cleanup(func() { compareHash = restore })

	const n = 5
	var wg sync.WaitGroup
	results := make([]bool, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = s.ComparePlainAccount(account.Username, "same-credential")
		}()
	}
	close(start)
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("goroutine %d: ComparePlainAccount = false, want true", i)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("bcrypt comparisons = %d, want 1: %d concurrent checks of the same credential must collapse to one, "+
			"otherwise each contends for the single core and every caller pays N x bcrypt", got, n)
	}
}

// TestBcryptGateStillChargesEachDistinctGuess is the security half of the
// pair above: collapsing identical credentials must not turn into
// deduplicating different ones. Every distinct guess still runs a full
// comparison, so serializing them costs an attacker exactly as much as
// before -- it only stops them thrashing the one core.
func TestBcryptGateStillChargesEachDistinctGuess(t *testing.T) {
	s := NewService(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	account, err := s.GetAccount()
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	var calls atomic.Int32
	restore := compareHash
	compareHash = func(_, _ []byte) error {
		calls.Add(1)
		return bcrypt.ErrMismatchedHashAndPassword // every guess is wrong
	}
	t.Cleanup(func() { compareHash = restore })

	const n = 4
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ComparePlainAccount(account.Username, string(rune('a'+i))+"-distinct-guess")
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != n {
		t.Errorf("bcrypt comparisons = %d, want %d: each distinct guess must pay full cost", got, n)
	}
}

// TestDefaultAccountHashIsComputedOnce guards the other half of the auth-latency
// problem found on the device: /etc/kvm/pwd does not exist on a fresh unit, so
// GetAccount falls through to getDefaultAccount on EVERY authentication. That
// used to run bcrypt.GenerateFromPassword each time -- a full-cost hash, thrown
// away immediately, on top of the CompareHashAndPassword that follows it. Two
// bcrypts per request instead of one, which on the single-core SoC is the
// difference between ~1.6s and ~3.2s of CPU before the 2s failure delay even
// starts.
//
// bcrypt salts randomly, so two independent generations differ. Identical
// output is therefore proof the hash was computed once and reused.
func TestDefaultAccountHashIsComputedOnce(t *testing.T) {
	first := getDefaultAccount()
	second := getDefaultAccount()

	if first.Password != second.Password {
		t.Error("getDefaultAccount re-hashed: the default account's bcrypt hash must be computed once " +
			"and reused, or every auth on a unit with no account file pays a wasted full-cost bcrypt")
	}
	if first.Username != "admin" || second.Username != "admin" {
		t.Errorf("default username changed: %q / %q", first.Username, second.Username)
	}
	// The reused hash must still actually verify, not just be stable.
	if err := bcrypt.CompareHashAndPassword([]byte(first.Password), []byte("admin")); err != nil {
		t.Errorf("memoized default hash no longer verifies: %v", err)
	}
}
