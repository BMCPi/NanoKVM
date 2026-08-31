package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"

	"golang.org/x/crypto/bcrypt"
)

const accountFile = "/etc/kvm/pwd"

type Account struct {
	Username string `json:"username"`
	Password string `json:"password"` // should be named HashedPassword for clarity
}

func (s *Service) GetAccount() (*Account, error) {
	if _, err := os.Stat(accountFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return getDefaultAccount(), nil
		}
		return nil, err
	}

	content, err := os.ReadFile(accountFile)
	if err != nil {
		return nil, err
	}

	var account Account
	if err = json.Unmarshal(content, &account); err != nil {
		s.log.Error("unmarshal account failed", slog.Any("err", err))
		return nil, err
	}

	return &account, nil
}

func (s *Service) SetAccount(username string, hashedPassword string) error {
	// G117 flags "password" as a secret being serialized. The value written is
	// a bcrypt digest, never a plaintext credential, and the JSON key is the
	// on-disk shape of /etc/kvm/pwd that every already-deployed device carries
	// — renaming it would make existing accounts unreadable.
	account, err := json.Marshal(&Account{ //nolint:gosec // G117: the marshaled value is a bcrypt digest and "password" is the established on-disk key
		Username: username,
		Password: hashedPassword,
	})
	if err != nil {
		s.log.Error("failed to marshal account information to json", slog.Any("err", err))
		return err
	}

	err = os.MkdirAll(filepath.Dir(accountFile), 0o644)
	if err != nil {
		s.log.Error("create directory failed", slog.String("path", accountFile), slog.Any("err", err))
		return err
	}

	// 0600: only this process (running as root) ever reads the credential
	// store, so there is nothing to gain from making the account hash
	// world-readable.
	err = os.WriteFile(accountFile, account, 0o600)
	if err != nil {
		s.log.Error("write password failed", slog.Any("err", err))
		return err
	}

	// Drop any memoized Basic-Auth credentials so the new password takes effect
	// immediately instead of after the cache TTL.
	s.cache.flush()
	return nil
}

func (s *Service) CompareAccount(username string, plainPassword string) bool {
	account, err := s.GetAccount()
	if err != nil {
		return false
	}

	if username != account.Username {
		return false
	}

	hashedPassword, err := utils.DecodeDecrypt(plainPassword, s.log)
	if err != nil || hashedPassword == "" {
		return false
	}

	err = bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(hashedPassword))
	if err != nil {
		// Compatible with old versions
		accountHashedPassword, _ := utils.DecodeDecrypt(account.Password, s.log)

		return accountHashedPassword == hashedPassword
	}

	return true
}

// ComparePlainAccount validates a username + plain-text password against
// the stored account. Used by standards-based protocols (Redfish, IPMI,
// HTTP Basic) where the client sends the password in plain text rather
// than the obfuscated form the web UI uses.
func (s *Service) ComparePlainAccount(username string, plainPassword string) bool {
	// Fast path: a recent successful check of this exact credential skips the
	// deliberately expensive bcrypt comparison. Only positives are cached and
	// the cache is flushed on any password change, so this never weakens
	// brute-force resistance (see basicauth_cache.go).
	if s.cache.get(username, plainPassword) {
		return true
	}

	account, err := s.GetAccount()
	if err != nil {
		return false
	}
	if username != account.Username {
		return false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.Password), []byte(plainPassword)); err == nil {
		s.cache.put(username, plainPassword)
		return true
	}
	// Legacy: older installs stored the password as an encrypted blob
	// rather than a bcrypt hash. Decrypt and compare directly.
	if stored, err := utils.DecodeDecrypt(account.Password, s.log); err == nil && stored == plainPassword {
		s.cache.put(username, plainPassword)
		return true
	}
	return false
}

func (s *Service) DelAccount() error {
	if err := os.Remove(accountFile); err != nil {
		s.log.Error("failed to delete password", slog.Any("err", err))
		return err
	}

	// The credential just changed (reset to default); drop memoized entries.
	s.cache.flush()
	return nil
}

func getDefaultAccount() *Account {
	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)

	return &Account{
		Username: "admin",
		Password: string(hashedPassword),
	}
}
