package ssh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The Go SFTP client proves the protocol; these tests prove the thing users
// actually type. OpenSSH 9.0 and later run scp over SFTP by default, so the
// subsystem is what makes `scp` work against a BMC that has no scp binary of
// its own — and only the real client can confirm the two agree.
//
// Skipped when the OpenSSH tools are not installed (CI images without them),
// the same way the ipmitool tests are gated.

func TestSCPWithOpenSSHClient(t *testing.T) {
	scp := lookupTool(t, "scp")
	client, keyPath := serverForOpenSSHTools(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "upload.bin")
	// Several 32 KiB SFTP packets' worth, so a botched continuation shows up.
	payload := bytes.Repeat([]byte("nanokvm-scp-"), 8192)
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// Upload: `scp file bmc:/path`.
	dst := filepath.Join(dir, "uploaded.bin")
	runTool(t, scp, sshToolArgs(keyPath, client, src, "root@127.0.0.1:"+dst)...)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("uploaded %d bytes, want %d", len(got), len(payload))
	}

	// Download: `scp bmc:/path file`.
	back := filepath.Join(dir, "downloaded.bin")
	runTool(t, scp, sshToolArgs(keyPath, client, "root@127.0.0.1:"+dst, back)...)

	got, err = os.ReadFile(back)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(payload))
	}
}

// TestSFTPWithOpenSSHClient covers the interactive tool and, with it, the
// directory operations scp -r leans on.
func TestSFTPWithOpenSSHClient(t *testing.T) {
	sftpTool := lookupTool(t, "sftp")
	client, keyPath := serverForOpenSSHTools(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "batch.txt")
	if err := os.WriteFile(src, []byte("batch-marker-ok\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	batch := filepath.Join(dir, "commands")
	dst := filepath.Join(dir, "sub", "batch.txt")
	script := strings.Join([]string{
		"mkdir " + filepath.Join(dir, "sub"),
		"put " + src + " " + dst,
		"ls " + filepath.Join(dir, "sub"),
		"rm " + dst,
		"rmdir " + filepath.Join(dir, "sub"),
	}, "\n") + "\n"
	if err := os.WriteFile(batch, []byte(script), 0o600); err != nil {
		t.Fatalf("write batch file: %v", err)
	}

	args := append(sshToolArgs(keyPath, client), "-b", batch, "root@127.0.0.1")
	out := runTool(t, sftpTool, args...)
	if !strings.Contains(out, "batch.txt") {
		t.Errorf("ls did not list the uploaded file:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Errorf("rmdir left the directory behind: %v", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func lookupTool(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found in PATH, skipping integration test", name)
	}
	return path
}

// serverForOpenSSHTools starts the server, authorizes a key, and writes that
// key where the OpenSSH tools can read it. It returns the listener address and
// the private key's path.
func serverForOpenSSHTools(t *testing.T) (addr string, keyPath string) {
	t.Helper()

	addr = startTestServer(t, false)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}

	keyPath = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	if err := WriteAuthorizedKeys(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}
	return addr, keyPath
}

// sshToolArgs builds the flags both tools need: our key only, no known_hosts
// bookkeeping for an ephemeral server, and no prompting.
func sshToolArgs(keyPath, addr string, rest ...string) []string {
	_, port, _ := net.SplitHostPort(addr)
	// scp spells the port -P, sftp spells it -P too (ssh is the odd one at -p).
	args := []string{
		"-P", port,
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
	}
	return append(args, rest...)
}

func runTool(t *testing.T, tool string, args ...string) string {
	t.Helper()

	cmd := exec.Command(tool, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(tool), strings.Join(args, " "), err, out)
	}
	return string(out)
}
