package ipmi

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	testUser = "admin"
	testPass = "admin"
)

// findFreePort returns an available UDP port.
func findFreePort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}

// startServer starts an IPMI server on a random port and returns the server and port.
func startServer(t *testing.T) (*Server, int) {
	t.Helper()
	port := findFreePort(t)
	srv, err := Start(port)
	if err != nil {
		t.Fatalf("start IPMI server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	// Give the listener goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)
	return srv, port
}

// ipmitoolPath returns the path to ipmitool, skipping if not found.
func ipmitoolPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ipmitool")
	if err != nil {
		t.Skip("ipmitool not found in PATH, skipping integration test")
	}
	return path
}

// runIPMITool executes ipmitool with common flags and returns combined output.
func runIPMITool(t *testing.T, port int, cipher int, args ...string) (string, error) {
	t.Helper()
	tool := ipmitoolPath(t)
	baseArgs := []string{
		"-I", "lanplus",
		"-H", "127.0.0.1",
		"-p", fmt.Sprintf("%d", port),
		"-U", testUser,
		"-P", testPass,
		"-C", fmt.Sprintf("%d", cipher),
	}
	fullArgs := append(baseArgs, args...)
	t.Logf("ipmitool %s", strings.Join(fullArgs, " "))

	cmd := exec.Command(tool, fullArgs...)
	out, err := cmd.CombinedOutput()
	output := string(out)
	t.Logf("output: %s", output)
	if err != nil {
		t.Logf("error: %v", err)
	}
	return output, err
}
