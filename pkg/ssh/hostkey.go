package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// hostKeyComment identifies keys this server generated, in the same
// user@host style OpenSSH uses.
const hostKeyComment = "nanokvm-bmc"

// loadOrCreateHostKey returns the server's host key signer, generating and
// persisting an ed25519 key on first start.
//
// The key must survive reboots or every reconnecting client gets a host-key
// mismatch warning, which is why the default path is on the data partition and
// not on the volatile root overlay.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil

	case errors.Is(err, os.ErrNotExist):
		return generateHostKey(path)

	default:
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}
}

func generateHostKey(path string) (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, hostKeyComment)
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ssh directory: %w", err)
	}
	// Write to a temp file and rename so a power cut mid-write can never leave
	// a truncated key that fails to parse on the next boot.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write host key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("install host key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("host key signer: %w", err)
	}

	slog.Info("ssh: generated ed25519 host key",
		slog.String("path", path),
		slog.String("fingerprint", ssh.FingerprintSHA256(signer.PublicKey())))
	return signer, nil
}
