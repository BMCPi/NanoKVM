package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// ed25519Key is a throwaway public key — generated for the test suite and
// never used to authenticate anything real.
const ed25519Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ6dCU0mPFhLGkTa3iRVJmtCPrYNiUDzMPzWMTXwLBFC test@example"

func TestValidateSSHKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{name: "ed25519", key: ed25519Key},
		{name: "with comments and blank lines", key: "# a comment\n\n" + ed25519Key + "\n\n"},
		{name: "multiple keys", key: ed25519Key + "\n" + ed25519Key},
		{name: "empty", key: "", wantErr: "no valid SSH key found"},
		{name: "only comments", key: "# nothing here\n", wantErr: "no valid SSH key found"},
		{name: "garbage", key: "this is not a key", wantErr: "no key found"},
		{name: "truncated base64", key: "ssh-ed25519 AAAAC3NzaC1lZDI1", wantErr: "no key found"},
		{
			name:    "type mismatch between text and blob",
			key:     "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIJ6dCU0mPFhLGkTa3iRVJmtCPrYNiUDzMPzWMTXwLBFC test@example",
			wantErr: "does not match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSSHKey(tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSSHKeyRejectsDSA guards the one type deliberately left out of
// ValidSSHKeyTypes: ssh-dss parses fine but must not be accepted.
func TestValidateSSHKeyRejectsDSA(t *testing.T) {
	if err := ValidateSSHKey("ssh-dss AAAAB3NzaC1kc3M= test@example"); err == nil {
		t.Fatal("expected ssh-dss to be rejected")
	}
}

// TestAuthorizedKeysRoundTrip covers the store: write, read back, parse, and
// clear. It points the config at a temp dir so nothing touches the real one.
func TestAuthorizedKeysRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "authorized_keys")
	conf := config.GetInstance()
	original := conf.SSH.AuthorizedKeysPath
	conf.SSH.AuthorizedKeysPath = path
	t.Cleanup(func() { conf.SSH.AuthorizedKeysPath = original })

	// Absent file reads as empty rather than erroring.
	if got, err := ReadAuthorizedKeys(); err != nil || got != "" {
		t.Fatalf("ReadAuthorizedKeys on missing file = %q, %v; want \"\", nil", got, err)
	}
	if keys := authorizedKeys(); len(keys) != 0 {
		t.Fatalf("authorizedKeys on missing file = %d keys, want 0", len(keys))
	}

	if err := WriteAuthorizedKeys(ed25519Key); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	got, err := ReadAuthorizedKeys()
	if err != nil {
		t.Fatalf("ReadAuthorizedKeys: %v", err)
	}
	if got != ed25519Key {
		t.Fatalf("ReadAuthorizedKeys = %q, want %q", got, ed25519Key)
	}

	if keys := authorizedKeys(); len(keys) != 1 {
		t.Fatalf("authorizedKeys = %d keys, want 1", len(keys))
	}

	// The file must not be world-readable — it is inside a 0700 directory but
	// its own mode matters if the dir already existed.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys mode = %o, want 600", fi.Mode().Perm())
	}

	// Invalid content is rejected without clobbering what is already stored.
	if err := WriteAuthorizedKeys("not a key"); err == nil {
		t.Error("expected invalid keys to be rejected")
	}
	if got, _ := ReadAuthorizedKeys(); got != ed25519Key {
		t.Errorf("stored keys were modified by a rejected write: %q", got)
	}

	// Empty clears the file.
	if err := WriteAuthorizedKeys(""); err != nil {
		t.Fatalf("WriteAuthorizedKeys(\"\"): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("authorized_keys still present after clear: %v", err)
	}
	// Clearing an already-absent file is not an error.
	if err := WriteAuthorizedKeys("  \n "); err != nil {
		t.Errorf("clearing an absent file: %v", err)
	}
}
