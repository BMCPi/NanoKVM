package ssh

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// ValidSSHKeyTypes is the list of accepted client public key types.
//
// Every entry must be verifiable by golang.org/x/crypto/ssh, which is the
// transport this server is built on. ssh-dss is deliberately absent: DSA is
// insecure and x/crypto/ssh no longer negotiates it by default.
//
// Ported from jetkvm-community/kvm internal/utils/ssh.go, where the same list
// was constrained by dropbear's signkey.c.
var ValidSSHKeyTypes = []string{
	ssh.KeyAlgoRSA,
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoSKED25519,
	ssh.KeyAlgoSKECDSA256,
}

// ValidateSSHKey validates authorized_keys file content. It succeeds when at
// least one line parses into a public key of an accepted type; blank lines and
// # comments are skipped. The error describes the last line that failed, so a
// user pasting one bad key gets a usable message.
func ValidateSSHKey(sshKey string) error {
	var (
		hasValidPublicKey = false
		lastError         = errors.New("no valid SSH key found")
	)

	for _, key := range strings.Split(sshKey, "\n") {
		key = strings.TrimSpace(key)

		// skip empty lines and comments
		if key == "" || strings.HasPrefix(key, "#") {
			continue
		}

		parsedPublicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
		if err != nil {
			lastError = err
			continue
		}

		if parsedPublicKey == nil {
			continue
		}

		parsedType := parsedPublicKey.Type()
		textType := strings.Fields(key)[0]

		if parsedType != textType {
			lastError = fmt.Errorf("parsed SSH key type %s does not match type in text %s", parsedType, textType)
			continue
		}

		if !slices.Contains(ValidSSHKeyTypes, parsedType) {
			lastError = fmt.Errorf("invalid SSH key type: %s", parsedType)
			continue
		}

		hasValidPublicKey = true
	}

	if !hasValidPublicKey {
		return lastError
	}

	return nil
}

// ReadAuthorizedKeys returns the authorized_keys file content, or "" when no
// keys have been configured yet.
func ReadAuthorizedKeys() (string, error) {
	path := config.GetInstance().SSH.AuthorizedKeysPath

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read authorized keys: %w", err)
	}
	return string(data), nil
}

// WriteAuthorizedKeys validates and stores authorized_keys content. Empty
// content removes the file, which disables public-key auth entirely.
func WriteAuthorizedKeys(sshKey string) error {
	path := config.GetInstance().SSH.AuthorizedKeysPath

	if strings.TrimSpace(sshKey) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove authorized keys: %w", err)
		}
		pkgLog().Info("ssh: authorized keys cleared")
		return nil
	}

	if err := ValidateSSHKey(sshKey); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ssh directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(sshKey), 0o600); err != nil {
		return fmt.Errorf("write authorized keys: %w", err)
	}

	pkgLog().Info("ssh: authorized keys updated")
	return nil
}

// authorizedKeys parses the configured authorized_keys file into public keys,
// skipping lines that do not parse. Read on every authentication attempt so an
// operator's edit takes effect without restarting the listener.
func authorizedKeys() []ssh.PublicKey {
	content, err := ReadAuthorizedKeys()
	if err != nil {
		pkgLog().Warn("ssh: read authorized keys failed", slog.Any("err", err))
		return nil
	}

	var keys []ssh.PublicKey
	rest := []byte(content)
	for len(rest) > 0 {
		key, _, _, remaining, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			// ParseAuthorizedKey stops at the first unparsable line and cannot
			// tell us where to resume, so the remainder is unusable.
			break
		}
		if key != nil && slices.Contains(ValidSSHKeyTypes, key.Type()) {
			keys = append(keys, key)
		}
		rest = remaining
	}
	return keys
}
