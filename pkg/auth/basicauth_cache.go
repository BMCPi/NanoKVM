package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// basicAuthCacheTTL bounds how long a successful plain-password check is
// remembered so repeated HTTP Basic / Redfish requests from the same client
// skip the (deliberately expensive) bcrypt comparison.
//
// bcrypt at DefaultCost is ~1s on this BMC's SoC, and standards-based Redfish
// clients (gofish, bmclib, the Dell Terraform provider) re-send Basic Auth on
// every request, so without this a polling client pays ~1s per call. Only
// *successful* checks are cached, so brute-force attempts still pay full bcrypt
// every time, and any password change flushes the cache (see
// invalidateBasicAuthCache) so a rotated or removed credential stops
// authenticating immediately rather than after the TTL.
const basicAuthCacheTTL = 5 * time.Minute

// authCache memoizes successful ComparePlainAccount results, keyed by a keyed
// digest of the presented credential — never the plaintext. The HMAC secret is
// random per process, so a cached key is opaque outside the running process and
// cannot be brute-forced offline the way an unsalted password hash could.
type authCache struct {
	mu      sync.Mutex
	secret  []byte
	entries map[string]time.Time // credential digest -> expiry
}

var basicAuthCache = newAuthCache()

func newAuthCache() *authCache {
	secret := make([]byte, 32)
	// crypto/rand.Read never short-reads; on the vanishingly unlikely error the
	// cache is still correct (keys are just no longer opaque), which is
	// acceptable for an in-RAM, short-lived entry.
	_, _ = rand.Read(secret)
	return &authCache{secret: secret, entries: make(map[string]time.Time)}
}

func (a *authCache) key(username, password string) string {
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(username))
	_, _ = mac.Write([]byte{0}) // domain-separate user from password
	_, _ = mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}

// get reports whether this exact credential was validated within the TTL.
func (a *authCache) get(username, password string) bool {
	k := a.key(username, password)
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.entries[k]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.entries, k)
		return false
	}
	return true
}

// put records a successful validation. Only call after a real bcrypt/legacy
// check has passed.
func (a *authCache) put(username, password string) {
	k := a.key(username, password)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	// Opportunistically drop expired entries so the map cannot accumulate stale
	// keys. There is normally only the one valid credential, but stay tidy.
	for key, exp := range a.entries {
		if now.After(exp) {
			delete(a.entries, key)
		}
	}
	a.entries[k] = now.Add(basicAuthCacheTTL)
}

func (a *authCache) flush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = make(map[string]time.Time)
}

// invalidateBasicAuthCache drops all memoized credentials. Called whenever the
// stored account changes so a rotated or removed password stops authenticating
// immediately.
func invalidateBasicAuthCache() {
	basicAuthCache.flush()
}
