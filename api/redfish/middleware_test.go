package redfish

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// HostTrace must record host-interface requests (the only boot-time record
// of what the managed host's firmware did) and stay silent for LAN traffic.
func TestHostTraceLogsHostInterfaceOnly(t *testing.T) {
	resetHostState(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(testDeps())
	r.GET("/redfish/v1/Systems/1/Bios/Settings", HostTrace(log), svc.GetBiosSettings)

	// The logger is constructed for this test alone (no slog.SetDefault), so
	// the buffer only ever sees what HostTrace itself writes — prove a LAN
	// request stays untraced.
	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/Settings", lanIP, "", nil); w.Code != http.StatusOK {
		t.Fatalf("LAN GET = %d", w.Code)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("LAN request logged: %q", got)
	}

	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/Settings", hostIP(t), "", nil); w.Code != http.StatusOK {
		t.Fatalf("host GET = %d", w.Code)
	}
	entries := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(entries) != 1 || entries[0] == "" {
		t.Fatalf("host request produced %d log entries, want 1: %q", len(entries), buf.String())
	}
	msg := entries[0]
	for _, want := range []string{"GET", "/redfish/v1/Systems/1/Bios/Settings", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("trace %q missing %q", msg, want)
		}
	}
}
