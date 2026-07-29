package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/pi-bmc/nanokvm-app/server/config"
)

// startTestServer brings the real server up on an ephemeral port with its
// state in a temp dir, and returns the address to dial.
func startTestServer(t *testing.T, passwordAuth bool) string {
	t.Helper()

	dir := t.TempDir()
	conf := config.GetInstance()
	saved := conf.SSH
	conf.SSH = config.SSH{
		Enabled:            true,
		Port:               0, // ephemeral
		HostKeyPath:        filepath.Join(dir, "ssh_host_ed25519_key"),
		AuthorizedKeysPath: filepath.Join(dir, "authorized_keys"),
		PasswordAuth:       passwordAuth,
	}
	t.Cleanup(func() {
		Stop()
		conf.SSH = saved
	})

	Stop() // ensure a clean slate if a previous test left one running
	if err := Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !IsRunning() {
		t.Fatal("server did not start")
	}
	return Addr()
}

// newClientKey generates a client key pair and authorizes its public half.
func newClientKey(t *testing.T) ssh.Signer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	return signer
}

func dial(t *testing.T, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	t.Helper()

	cfg.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	cfg.Timeout = 10 * time.Second
	return ssh.Dial("tcp", addr, cfg)
}

// TestPublicKeyAuthAndExec is the end-to-end path: authorize a key, connect
// with it, run a command, and check stdout and the exit status.
func TestPublicKeyAuthAndExec(t *testing.T) {
	addr := startTestServer(t, false)
	signer := newClientKey(t)

	authorized := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	if err := WriteAuthorizedKeys(authorized); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	client, err := dial(t, addr, &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()

	out, err := session.Output("echo exec-marker-ok")
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if !strings.Contains(string(out), "exec-marker-ok") {
		t.Fatalf("output = %q, want exec-marker-ok", out)
	}
}

// TestExecExitStatus checks that a non-zero exit reaches the client, which is
// what any script driving the BMC over SSH depends on.
func TestExecExitStatus(t *testing.T) {
	addr := startTestServer(t, false)
	signer := newClientKey(t)
	if err := WriteAuthorizedKeys(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	client, err := dial(t, addr, &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()

	err = session.Run("exit 3")
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v (%T), want *ssh.ExitError", err, err)
	}
	if exitErr.ExitStatus() != 3 {
		t.Errorf("exit status = %d, want 3", exitErr.ExitStatus())
	}
}

// TestInteractiveShellWithPTY covers the pty-req path an `ssh bmc` login takes:
// a terminal is allocated, TERM and the window size reach the shell, and typed
// input round-trips.
func TestInteractiveShellWithPTY(t *testing.T) {
	addr := startTestServer(t, false)
	signer := newClientKey(t)
	if err := WriteAuthorizedKeys(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	client, err := dial(t, addr, &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.RequestPty("xterm-256color", 40, 123, ssh.TerminalModes{}); err != nil {
		t.Fatalf("pty-req: %v", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	if _, err := stdin.Write([]byte("echo tty=$(tty | grep -c pts) size=${COLUMNS}x${LINES} term=$TERM\nexit\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := readUntilDeadline(t, stdout, "term=xterm-256color")
	if !strings.Contains(got, "tty=1") {
		t.Errorf("session is not on a pts device: %q", got)
	}
	if !strings.Contains(got, "size=123x40") {
		t.Errorf("window size not applied: %q", got)
	}
	if !strings.Contains(got, "term=xterm-256color") {
		t.Errorf("TERM not propagated: %q", got)
	}
}

// TestPasswordAuth uses the BMC account credentials (the default admin/admin
// when no account file exists), the same ones Redfish and IPMI accept.
func TestPasswordAuth(t *testing.T) {
	if _, err := os.Stat("/etc/kvm/pwd"); err == nil {
		t.Skip("a real account file exists on this host; skipping default-credential test")
	}

	addr := startTestServer(t, true)

	client, err := dial(t, addr, &ssh.ClientConfig{
		User: "admin",
		Auth: []ssh.AuthMethod{ssh.Password("admin")},
	})
	if err != nil {
		t.Fatalf("dial with password: %v", err)
	}
	_ = client.Close()

	// Wrong password must be refused.
	if c, err := dial(t, addr, &ssh.ClientConfig{
		User: "admin",
		Auth: []ssh.AuthMethod{ssh.Password("not-the-password")},
	}); err == nil {
		_ = c.Close()
		t.Fatal("expected authentication failure for a wrong password")
	}
}

// TestPasswordAuthDisabled confirms the config switch actually removes the
// password method from the handshake.
func TestPasswordAuthDisabled(t *testing.T) {
	addr := startTestServer(t, false)

	if c, err := dial(t, addr, &ssh.ClientConfig{
		User: "admin",
		Auth: []ssh.AuthMethod{ssh.Password("admin")},
	}); err == nil {
		_ = c.Close()
		t.Fatal("expected password auth to be refused when disabled")
	}
}

// TestUnauthorizedKeyRejected is the negative case for key auth: a valid key
// that is not in authorized_keys must not get in.
func TestUnauthorizedKeyRejected(t *testing.T) {
	addr := startTestServer(t, false)

	authorized := newClientKey(t)
	if err := WriteAuthorizedKeys(string(ssh.MarshalAuthorizedKey(authorized.PublicKey()))); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	stranger := newClientKey(t)
	if c, err := dial(t, addr, &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(stranger)},
	}); err == nil {
		_ = c.Close()
		t.Fatal("expected an unauthorized key to be rejected")
	}
}

// TestHostKeyIsStableAcrossRestarts is the reason the key lives on the data
// partition: a client must not see a host-key change when the BMC reboots.
func TestHostKeyIsStableAcrossRestarts(t *testing.T) {
	addr := startTestServer(t, false)

	first := hostKeyOf(t, addr)
	if err := Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	second := hostKeyOf(t, Addr())

	if first != second {
		t.Fatalf("host key changed across restart: %s -> %s", first, second)
	}
	if !strings.HasPrefix(first, "SHA256:") {
		t.Errorf("unexpected fingerprint format: %s", first)
	}
}

// TestGeneratedHostKeyIsPrivate checks the on-disk permissions of the key the
// server generates on first start.
func TestGeneratedHostKeyIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "ssh_host_ed25519_key")

	signer, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateHostKey: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("host key type = %s, want %s", signer.PublicKey().Type(), ssh.KeyAlgoED25519)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("host key mode = %o, want 600", fi.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if block, _ := pem.Decode(data); block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		t.Errorf("host key is not an OpenSSH PEM private key")
	}

	// A second load must reuse the key, not mint a new one.
	again, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if ssh.FingerprintSHA256(signer.PublicKey()) != ssh.FingerprintSHA256(again.PublicKey()) {
		t.Error("host key was regenerated instead of reused")
	}
}

// TestStopClosesListener makes sure the disable path actually frees the port.
func TestStopClosesListener(t *testing.T) {
	addr := startTestServer(t, false)
	Stop()

	if IsRunning() {
		t.Fatal("IsRunning after Stop")
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("listener still accepting after Stop")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func hostKeyOf(t *testing.T, addr string) string {
	t.Helper()

	var fingerprint string
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.Password("wrong-on-purpose")},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 10 * time.Second,
	})
	if client != nil {
		_ = client.Close()
	}
	// Authentication is expected to fail; the host key is captured during the
	// handshake that precedes it.
	if fingerprint == "" {
		t.Fatalf("no host key seen: %v", err)
	}
	return fingerprint
}

func readUntilDeadline(t *testing.T, r interface{ Read([]byte) (int, error) }, marker string) string {
	t.Helper()

	type result struct{ s string }
	out := make(chan result, 1)
	go func() {
		var sb strings.Builder
		b := make([]byte, 512)
		for {
			n, err := r.Read(b)
			if n > 0 {
				sb.Write(b[:n])
				if strings.Contains(sb.String(), marker) {
					out <- result{sb.String()}
					return
				}
			}
			if err != nil {
				out <- result{sb.String()}
				return
			}
		}
	}()

	select {
	case res := <-out:
		return res.s
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %q", marker)
		return ""
	}
}
