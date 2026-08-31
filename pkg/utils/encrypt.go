package utils

import (
	"log/slog"
	"net/url"

	"github.com/mervick/aes-everywhere/go/aes256"
)

// SecretKey is only used to prevent the data from being transmitted in plaintext.
//
// It is not a credential and it protects nothing: it is the shared obfuscation
// key the NanoKVM login protocol has always used, hardcoded identically in the
// vendor's own browser client (and read back out of this very line by the
// Makefile's deploy recipe, which is why the directive below must stay on its
// own line — a trailing comment would break that sed). Its entire job is to
// keep a login form field from travelling as literal plaintext inside a
// request TLS has already encrypted. Rotating it, randomising it per device,
// or moving it to config would break login for every existing client without
// making anything harder for an attacker, who reads it out of the client.
//
//nolint:gosec // G101: shared obfuscation constant baked into every client, not a credential
const SecretKey = "nanokvm-sipeed-2024"

func Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	decrypt := aes256.Decrypt(ciphertext, SecretKey)
	return decrypt, nil
}

func DecodeDecrypt(data string) (string, error) {
	ciphertext, err := url.QueryUnescape(data)
	if err != nil {
		slog.Error("decode ciphertext failed", slog.Any("err", err))
		return "", err
	}

	return Decrypt(ciphertext)
}
