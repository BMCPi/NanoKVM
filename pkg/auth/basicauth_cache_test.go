package auth

import (
	"os"
	"testing"
	"time"
)

func TestAuthCacheHitAndIsolation(t *testing.T) {
	c := newAuthCache()

	if c.get("admin", "secret") {
		t.Fatal("empty cache reported a hit")
	}

	c.put("admin", "secret")
	if !c.get("admin", "secret") {
		t.Fatal("expected a hit for the credential just stored")
	}

	// A different password or username must not hit the same entry.
	if c.get("admin", "secret2") {
		t.Fatal("different password should miss")
	}
	if c.get("root", "secret") {
		t.Fatal("different username should miss")
	}
}

func TestAuthCacheExpiry(t *testing.T) {
	c := newAuthCache()
	c.put("admin", "secret")

	// Force the entry to be expired without waiting out the TTL.
	c.mu.Lock()
	for k := range c.entries {
		c.entries[k] = time.Now().Add(-time.Second)
	}
	c.mu.Unlock()

	if c.get("admin", "secret") {
		t.Fatal("expired entry should miss")
	}
	// The expired entry should have been pruned on the miss.
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired entry not pruned, %d remain", n)
	}
}

func TestAuthCacheFlush(t *testing.T) {
	c := newAuthCache()
	c.put("admin", "secret")
	c.flush()
	if c.get("admin", "secret") {
		t.Fatal("flush did not clear the entry")
	}
}

// TestAuthCacheKeyOpaque verifies the cache key does not embed the plaintext
// credential and that two caches (different random secrets) key differently.
func TestAuthCacheKeyOpaque(t *testing.T) {
	c := newAuthCache()
	k := c.key("admin", "hunter2")
	if k == "" {
		t.Fatal("empty key")
	}
	for _, s := range []string{"admin", "hunter2"} {
		if containsSubstr(k, s) {
			t.Fatalf("cache key leaks plaintext %q", s)
		}
	}
	if c2 := newAuthCache(); c2.key("admin", "hunter2") == k {
		t.Fatal("keys collide across independent per-process secrets")
	}
}

func containsSubstr(haystack, needle string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestComparePlainAccountFastPath exercises the integration with the default
// admin/admin account. It is skipped when a real account file is present so it
// never depends on machine-specific credentials.
func TestComparePlainAccountFastPath(t *testing.T) {
	if _, err := os.Stat(AccountFile); err == nil {
		t.Skipf("%s exists; skipping default-account fast-path test", AccountFile)
	}
	basicAuthCache.flush()
	t.Cleanup(basicAuthCache.flush)

	// First (uncached) check runs bcrypt and, on success, populates the cache.
	if !ComparePlainAccount("admin", "admin") {
		t.Fatal("default admin/admin should validate")
	}
	if !basicAuthCache.get("admin", "admin") {
		t.Fatal("successful check was not cached")
	}

	// The cached hit is dramatically faster than a bcrypt compare.
	start := time.Now()
	ok := ComparePlainAccount("admin", "admin")
	elapsed := time.Since(start)
	if !ok {
		t.Fatal("cached check should still validate")
	}
	if elapsed > 10*time.Millisecond {
		t.Fatalf("cached check took %v; expected sub-millisecond (bcrypt not skipped?)", elapsed)
	}

	// A wrong password must fail and must not be cached.
	if ComparePlainAccount("admin", "wrong") {
		t.Fatal("wrong password should fail")
	}
	if basicAuthCache.get("admin", "wrong") {
		t.Fatal("failed check must not be cached")
	}

	// Invalidation clears the memoized credential.
	invalidateBasicAuthCache()
	if basicAuthCache.get("admin", "admin") {
		t.Fatal("invalidate did not clear the cache")
	}
}
