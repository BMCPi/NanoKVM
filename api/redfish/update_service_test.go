package redfish

// update_service_test.go covers the two ways a Redfish client hands the BMC a
// UEFI FMP capsule. The end-to-end assertion that matters is not the HTTP
// status: it is that the bytes land in \EFI\UpdateCapsule\ on the capsule
// volume, because that directory is the entire contract with the host's
// firmware.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

// updateServiceRouter mounts the UpdateService surface against a Firmware
// controller whose capsule volume lives in t's temp dir.
func updateServiceRouter(t *testing.T) (*gin.Engine, *firmware.Controller) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Firmware.CapsulePath = filepath.Join(t.TempDir(), "capsules.img")
	cfg.Firmware.CapsuleSizeMB = 48
	fw := firmware.NewController(cfg, slog.New(slog.DiscardHandler))

	h := &handlers{
		d: &deps.Deps{
			Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
			Firmware: fw,
		},
		log:   slog.New(slog.DiscardHandler),
		tasks: newTaskRegistry(),
	}
	r := gin.New()
	r.GET(updateServicePath, h.GetUpdateService)
	r.POST(simpleUpdatePath, h.SimpleUpdate)
	r.POST(httpPushURIPath, h.PushCapsule)
	// The task monitor a 202's Location points at — polled by the new
	// SimpleUpdate tests exactly the way redfish_command/gofish would.
	r.GET(tasksPath+"/:id", h.GetTask)
	return r, fw
}

func TestUpdateServiceAdvertisesPushURI(t *testing.T) {
	r, _ := updateServiceRouter(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, updateServicePath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET UpdateService = %d, want 200", w.Code)
	}

	var body struct {
		ServiceEnabled bool   `json:"ServiceEnabled"`
		HTTPPushURI    string `json:"HttpPushUri"`
		Actions        struct {
			SimpleUpdate struct {
				Target string `json:"target"`
			} `json:"#UpdateService.SimpleUpdate"`
		} `json:"Actions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode UpdateService: %v", err)
	}
	if !body.ServiceEnabled {
		t.Error("ServiceEnabled = false, want true")
	}
	if body.HTTPPushURI != httpPushURIPath {
		t.Errorf("HttpPushUri = %q, want %q", body.HTTPPushURI, httpPushURIPath)
	}
	if body.Actions.SimpleUpdate.Target != simpleUpdatePath {
		t.Errorf("SimpleUpdate target = %q, want %q", body.Actions.SimpleUpdate.Target, simpleUpdatePath)
	}
}

func TestPushCapsuleRawBodyStagesIt(t *testing.T) {
	r, fw := updateServiceRouter(t)

	payload := bytes.Repeat([]byte("FMP-CAPSULE."), 512)
	req := httptest.NewRequest(http.MethodPost, httpPushURIPath+"?name=host.cap", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("push = %d (%s), want 202", w.Code, w.Body.String())
	}
	assertStaged(t, fw, "host.cap", int64(len(payload)))
}

func TestPushCapsuleMultipartStagesIt(t *testing.T) {
	r, fw := updateServiceRouter(t)

	payload := bytes.Repeat([]byte("MULTIPART."), 256)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("UpdateFile", "bios-2026.08.cap")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, httpPushURIPath, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("push = %d (%s), want 202", w.Code, w.Body.String())
	}
	assertStaged(t, fw, "bios-2026.08.cap", int64(len(payload)))
}

// A raw push with no name still has to land somewhere firmware will look,
// under a stable name so repeated pushes replace rather than accumulate.
func TestPushCapsuleWithoutNameUsesDefault(t *testing.T) {
	r, fw := updateServiceRouter(t)

	req := httptest.NewRequest(http.MethodPost, httpPushURIPath, strings.NewReader("capsule"))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("push = %d (%s), want 202", w.Code, w.Body.String())
	}
	assertStaged(t, fw, "update.cap", int64(len("capsule")))
}

// SimpleUpdate has no implicit "latest": a bodyless call must be rejected, not
// silently push something stale at the host.
func TestSimpleUpdateRequiresImageURI(t *testing.T) {
	r, fw := updateServiceRouter(t)

	for _, tc := range []struct{ name, body, contentType string }{
		{"no body", "", ""},
		{"empty json", "{}", "application/json"},
		{"empty ImageURI", `{"ImageURI":""}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, simpleUpdatePath, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("SimpleUpdate = %d (%s), want 400", w.Code, w.Body.String())
			}
		})
	}

	// Nothing was staged, so a host rebooting now applies nothing.
	capsules, err := fw.ListCapsules()
	if err == nil && len(capsules) != 0 {
		t.Errorf("rejected SimpleUpdate left %+v staged", capsules)
	}
}

// A 202 with nothing to poll is what this feature retires: SimpleUpdate must
// hand back a Location an operator tool can watch, and the task must reach
// Completed once the capsule is staged — with the old UpdateInProgress
// message folded into Task.Messages so no information is lost.
func TestSimpleUpdateReturnsTaskMonitorAndCompletes(t *testing.T) {
	r, fw := updateServiceRouter(t)

	payload := bytes.Repeat([]byte("FMP-CAPSULE."), 512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare the length the way a real file server does — otherwise a
		// body past Go's write buffer goes chunked and progress has no total.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, simpleUpdatePath,
		strings.NewReader(`{"ImageURI":"`+srv.URL+`/host.cap"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("SimpleUpdate = %d (%s), want 202", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, tasksPath+"/") {
		t.Fatalf("Location = %q, want a task under %s", loc, tasksPath)
	}
	var accepted map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("202 body: %v", err)
	}
	if got, _ := accepted["@odata.id"].(string); got != loc {
		t.Errorf("202 body @odata.id = %q, want the Location %q", got, loc)
	}
	var folded bool
	for _, m := range taskMessages(t, accepted) {
		if m["MessageId"] == "Update.1.0.UpdateInProgress" {
			folded = true
		}
	}
	if !folded {
		t.Errorf("202 task Messages = %v, want Update.1.0.UpdateInProgress folded in", accepted["Messages"])
	}

	final := waitForTask(t, r, loc)
	if got, _ := final["TaskState"].(string); got != "Completed" {
		t.Fatalf("TaskState = %q (%v), want Completed", got, final)
	}
	if got, _ := final["TaskStatus"].(string); got != "OK" {
		t.Errorf("TaskStatus = %q, want OK", got)
	}
	// The remote declared a Content-Length, so progress was measurable and
	// must have reached 100.
	if got, _ := final["PercentComplete"].(float64); got != 100 {
		t.Errorf("PercentComplete = %v, want 100", final["PercentComplete"])
	}
	assertStaged(t, fw, "host.cap", int64(len(payload)))
}

// A staging failure used to be visible only in the BMC log; now the task the
// 202 pointed at must carry it.
func TestSimpleUpdateFailedFetchDrivesTaskToException(t *testing.T) {
	r, fw := updateServiceRouter(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such capsule", http.StatusInternalServerError)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, simpleUpdatePath,
		strings.NewReader(`{"ImageURI":"`+srv.URL+`/missing.cap"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("SimpleUpdate = %d (%s), want 202", w.Code, w.Body.String())
	}
	final := waitForTask(t, r, w.Header().Get("Location"))
	if got, _ := final["TaskState"].(string); got != "Exception" {
		t.Fatalf("TaskState = %q, want Exception", got)
	}
	if got, _ := final["TaskStatus"].(string); got != "Critical" {
		t.Errorf("TaskStatus = %q, want Critical", got)
	}
	var carried bool
	for _, m := range taskMessages(t, final) {
		if msg, _ := m["Message"].(string); strings.Contains(msg, "500") {
			carried = true
		}
	}
	if !carried {
		t.Errorf("Messages = %v, want the fetch error carried to the operator", final["Messages"])
	}

	capsules, err := fw.ListCapsules()
	if err == nil && len(capsules) != 0 {
		t.Errorf("failed staging left %+v behind", capsules)
	}
}

// assertStaged checks that exactly one capsule is queued for the host, under
// the given name and size.
func assertStaged(t *testing.T, fw *firmware.Controller, name string, size int64) {
	t.Helper()
	capsules, err := fw.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(capsules) != 1 {
		t.Fatalf("ListCapsules = %+v, want exactly one capsule", capsules)
	}
	if capsules[0].Name != name {
		t.Errorf("staged capsule = %q, want %q", capsules[0].Name, name)
	}
	if capsules[0].Size != size {
		t.Errorf("staged capsule size = %d, want %d", capsules[0].Size, size)
	}
}
