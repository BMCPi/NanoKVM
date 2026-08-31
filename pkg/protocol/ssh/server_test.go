package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/protocol/shell"
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
	if err := Start(slog.New(slog.DiscardHandler), auth.NewService(context.Background(), slog.New(slog.DiscardHandler))); err != nil {
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

// authorizedClient starts a server, authorizes a fresh key, and returns a
// connected client — the setup every session test needs before it can do
// anything interesting.
func authorizedClient(t *testing.T) *ssh.Client {
	t.Helper()

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
	t.Cleanup(func() { _ = client.Close() })
	return client
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

// TestSFTPSubsystem is the file-transfer path `scp` takes on OpenSSH 9.0 and
// later: request the sftp subsystem, then round-trip a file through it.
func TestSFTPSubsystem(t *testing.T) {
	client := authorizedClient(t)

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp subsystem: %v", err)
	}
	defer func() { _ = sftpClient.Close() }()

	path := filepath.Join(t.TempDir(), "payload.bin")
	// Large enough to span several 32 KiB SFTP packets, so a short write or a
	// mishandled continuation shows up here rather than on the board.
	want := bytes.Repeat([]byte("nanokvm-sftp-"), 8192)

	w, err := sftpClient.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close after write: %v", err)
	}

	// Read it back through SFTP...
	r, err := sftpClient.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-tripped %d bytes, want %d (equal: %t)", len(got), len(want), bytes.Equal(got, want))
	}

	// ...and confirm the bytes really landed on disk, not just in the server.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read from disk: %v", err)
	}
	if !bytes.Equal(onDisk, want) {
		t.Errorf("file on disk is %d bytes, want %d", len(onDisk), len(want))
	}

	fi, err := sftpClient.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != int64(len(want)) {
		t.Errorf("stat size = %d, want %d", fi.Size(), len(want))
	}

	if err := sftpClient.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file survived removal: %v", err)
	}
}

// TestSFTPWorkingDirectory pins where a relative path lands. `scp file bmc:`
// must write to the shell's home directory the way sshd behaves, not to the
// server process's cwd — whatever directory init happened to launch it from.
func TestSFTPWorkingDirectory(t *testing.T) {
	client := authorizedClient(t)

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp subsystem: %v", err)
	}
	defer func() { _ = sftpClient.Close() }()

	cwd, err := sftpClient.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if cwd != shell.Dir() {
		t.Errorf("sftp working directory = %q, want %q", cwd, shell.Dir())
	}
}

// TestUnknownSubsystemRejected keeps the subsystem door open only for SFTP.
func TestUnknownSubsystemRejected(t *testing.T) {
	client := authorizedClient(t)

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.RequestSubsystem("netconf"); err == nil {
		t.Fatal("expected an unknown subsystem to be refused")
	}
}

// TestMalformedSubsystemRequest feeds the subsystem handler a payload too
// short to hold its length-prefixed name. pkg/sftp's own example server reads
// the name as req.Payload[4:], which panics on this input and would take the
// whole BMC process down with it; parsing the payload properly must simply
// refuse the request and leave the connection usable.
func TestMalformedSubsystemRequest(t *testing.T) {
	client := authorizedClient(t)

	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	go ssh.DiscardRequests(reqs)

	ok, err := ch.SendRequest("subsystem", true, []byte{0x00, 0x01})
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	if ok {
		t.Error("a truncated subsystem request was accepted")
	}
	_ = ch.Close()

	// The server must still be serving: a panic in the request loop would
	// have taken the process down, and a lesser mishandling the connection.
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session after malformed request: %v", err)
	}
	defer func() { _ = session.Close() }()

	out, err := session.Output("echo still-alive")
	if err != nil {
		t.Fatalf("run command after malformed request: %v", err)
	}
	if !strings.Contains(string(out), "still-alive") {
		t.Errorf("output = %q, want still-alive", out)
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

	signer, err := loadOrCreateHostKey(path, slog.New(slog.DiscardHandler))
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
	again, err := loadOrCreateHostKey(path, slog.New(slog.DiscardHandler))
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
