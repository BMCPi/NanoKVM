package redfish

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus/hooks/test"
)

// HostTrace must record host-interface requests (the only boot-time record
// of what the managed host's firmware did) and stay silent for LAN traffic.
func TestHostTraceLogsHostInterfaceOnly(t *testing.T) {
	resetHostState(t)
	hook := test.NewGlobal()
	defer hook.Reset()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(testDeps())
	r.GET("/redfish/v1/Systems/1/Bios/Settings", HostTrace(), svc.GetBiosSettings)

	// First request also lazily initializes config, which logs unrelated
	// startup noise — clear the hook before asserting, then prove a LAN
	// request stays untraced.
	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/Settings", lanIP, "", nil); w.Code != http.StatusOK {
		t.Fatalf("LAN GET = %d", w.Code)
	}
	hook.Reset()
	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/Settings", lanIP, "", nil); w.Code != http.StatusOK {
		t.Fatalf("LAN GET = %d", w.Code)
	}
	if len(hook.Entries) != 0 {
		t.Fatalf("LAN request logged: %v", hook.Entries)
	}

	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/Settings", hostIP(t), "", nil); w.Code != http.StatusOK {
		t.Fatalf("host GET = %d", w.Code)
	}
	if len(hook.Entries) != 1 {
		t.Fatalf("host request produced %d log entries, want 1", len(hook.Entries))
	}
	msg := hook.LastEntry().Message
	for _, want := range []string{"GET", "/redfish/v1/Systems/1/Bios/Settings", "200"} {
		if !strings.Contains(msg, want) {
			t.Errorf("trace %q missing %q", msg, want)
		}
	}
}
