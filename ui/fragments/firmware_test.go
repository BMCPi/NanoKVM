package fragments

// firmware_test.go covers the Firmware settings panel: the capsule queue the
// host drains at its next boot, and the four ways an operator changes it.
//
// The upload test is the important one, and it is the same regression
// media_test.go guards: a multipart handler written with c.FormFile /
// ParseMultipartForm spools the whole body into os.TempDir() before the
// handler sees a byte, and on this board os.TempDir() is a RAM overlay. A
// firmware capsule is smaller than an ISO but not small — 128 MiB is allowed —
// so the same failure is reachable here. The assertion is therefore about
// *where the bytes go*, not the HTTP status.
//
// probingBody and tempDirNames are shared with media_test.go.

import (
	"bytes"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// firmwareRouter mounts the firmware fragment routes against a Controller
// whose capsule volume lives in t's temp dir.
//
// The volume is sized at the configured maximum rather than the default,
// because the streaming probe below has to push a capsule past gin's 32 MiB
// in-memory multipart budget and it still has to fit on the FAT.
func firmwareRouter(t *testing.T) (*gin.Engine, *firmware.Controller) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Firmware.CapsulePath = filepath.Join(t.TempDir(), "capsules.img")
	cfg.Firmware.CapsuleSizeMB = 256
	ctrl := firmware.NewController(cfg, slog.New(slog.DiscardHandler))
	d := &deps.Deps{Config: cfg, Firmware: ctrl}

	r := gin.New()
	r.Use(deps.Middleware(d))
	firmwareFragmentRoutes(r.Group("/ui"), testHandlers(d))
	return r, ctrl
}

// capsuleUploadBody builds a multipart body carrying one capsule.
func capsuleUploadBody(t *testing.T, filename string, payload []byte) ([]byte, string) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// stageCapsule puts a capsule on the volume the way a completed upload would,
// so the read-side tests do not depend on the upload handler.
func stageCapsule(t *testing.T, ctrl *firmware.Controller, name, content string) {
	t.Helper()
	if _, err := ctrl.StageCapsule(name, strings.NewReader(content)); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
}

func capsuleNames(t *testing.T, ctrl *firmware.Controller) []string {
	t.Helper()
	capsules, err := ctrl.ListCapsules()
	if err != nil {
		t.Fatalf("list capsules: %v", err)
	}
	names := make([]string, 0, len(capsules))
	for _, c := range capsules {
		names = append(names, c.Name)
	}
	return names
}

func TestCapsuleUploadStreamsToTheVolumeWithoutSpooling(t *testing.T) {
	r, ctrl := firmwareRouter(t)

	// TMPDIR isolated so "did anything spool?" is unambiguous. os.TempDir()
	// honours $TMPDIR on Unix.
	t.Setenv("TMPDIR", t.TempDir())

	// Past the 32 MiB a multipart form holds in memory before spooling, so a
	// handler written the buffering way is guaranteed to have a temp file open
	// by the time the probe fires.
	payload := bytes.Repeat([]byte("nanokvm-fmp-capsule-"), 1_800_000) // 36 MB
	body, contentType := capsuleUploadBody(t, "host.cap", payload)

	var midFlight []string
	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules", nil)
	req.Header.Set("Content-Type", contentType)
	req.Body = &probingBody{
		r: bytes.NewReader(body),
		// 90% in: spooling only starts once the in-memory budget is spent, so
		// an earlier probe would miss it.
		at:    len(body) * 9 / 10,
		probe: func() { midFlight = tempDirNames(t) },
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if midFlight == nil {
		t.Fatal("probe never fired: the request body was not read incrementally")
	}
	if len(midFlight) != 0 {
		t.Errorf("upload spooled into os.TempDir() mid-flight: %v\n"+
			"on the BMC that directory is RAM; the body must stream onto the capsule volume", midFlight)
	}
	if after := tempDirNames(t); len(after) != 0 {
		t.Errorf("upload left files in os.TempDir(): %v", after)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	capsules, err := ctrl.ListCapsules()
	if err != nil {
		t.Fatalf("list capsules: %v", err)
	}
	if len(capsules) != 1 || capsules[0].Name != "host.cap" {
		t.Fatalf("staged capsules = %+v, want one named host.cap", capsules)
	}
	if capsules[0].Size != int64(len(payload)) {
		t.Errorf("staged %d bytes, want %d", capsules[0].Size, len(payload))
	}
}

// Capsules are never decompressed (see the package comment on
// firmwareFetchStatus / StageCapsule) — unlike a virtual-media upload, the
// completion toast here must report a size and nothing else: no codec, no
// "extracted from", nothing implying a capsule was ever inflated.
func TestCapsuleUploadReportsSizeWithoutExtractionWording(t *testing.T) {
	r, ctrl := firmwareRouter(t)

	payload := []byte("firmware capsule payload bytes, staged as-is")
	body, contentType := capsuleUploadBody(t, "host.cap", payload)

	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	trigger := w.Header().Get("HX-Trigger")
	if strings.Contains(trigger, "extract") {
		t.Errorf("HX-Trigger = %q, a capsule is never decompressed and must not claim extraction", trigger)
	}
	if !strings.Contains(trigger, fmt.Sprintf("%d B", len(payload))) {
		t.Errorf("HX-Trigger = %q, want the toast to report the staged size", trigger)
	}
	if !strings.Contains(trigger, "Applies at the host's next boot.") {
		t.Errorf("HX-Trigger = %q, want the existing next-boot framing preserved", trigger)
	}

	if names := capsuleNames(t, ctrl); len(names) != 1 || names[0] != "host.cap" {
		t.Errorf("capsules = %v, want [host.cap]", names)
	}
}

// An upload past the cap must be refused, and must leave nothing half-written
// for the host's firmware to trip over.
func TestCapsuleUploadRejectsOversize(t *testing.T) {
	r, ctrl := firmwareRouter(t)

	// Shrink the cap rather than sending 128 MiB: the property under test is
	// that the cap is enforced, not how big it is.
	orig := maxCapsuleUploadBytes
	maxCapsuleUploadBytes = 4 << 10
	t.Cleanup(func() { maxCapsuleUploadBytes = orig })

	payload := bytes.Repeat([]byte("x"), 64<<10)
	body, contentType := capsuleUploadBody(t, "too-big.cap", payload)

	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "error") {
		t.Errorf("HX-Trigger = %q, want an error toast reporting the oversize upload", trigger)
	}
	if names := capsuleNames(t, ctrl); len(names) != 0 {
		t.Errorf("oversize upload left %v staged; nothing must reach the host", names)
	}
}

func TestCapsuleDeleteRemovesOnlyThatCapsule(t *testing.T) {
	r, ctrl := firmwareRouter(t)
	stageCapsule(t, ctrl, "keep.cap", "keep")
	stageCapsule(t, ctrl, "drop.cap", "drop")

	req := httptest.NewRequest(http.MethodDelete, "/ui/settings/firmware/capsules/drop.cap", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	names := capsuleNames(t, ctrl)
	if len(names) != 1 || names[0] != "keep.cap" {
		t.Fatalf("capsules after delete = %v, want [keep.cap]", names)
	}
	// The answer is the re-rendered panel, so the operator sees the queue that
	// actually exists rather than the one they asked for.
	if !strings.Contains(w.Body.String(), "keep.cap") {
		t.Error("delete must answer with the re-rendered panel listing what is still queued")
	}
}

// A traversing name must not be able to reach past the capsule directory.
func TestCapsuleDeleteRejectsTraversal(t *testing.T) {
	r, ctrl := firmwareRouter(t)
	stageCapsule(t, ctrl, "keep.cap", "keep")

	req := httptest.NewRequest(http.MethodDelete, "/ui/settings/firmware/capsules/..%2F..%2Fkeep.cap", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	if names := capsuleNames(t, ctrl); len(names) != 1 || names[0] != "keep.cap" {
		t.Fatalf("capsules = %v, want the traversing delete to have changed nothing", names)
	}
}

func TestCapsuleClearEmptiesTheQueue(t *testing.T) {
	r, ctrl := firmwareRouter(t)
	stageCapsule(t, ctrl, "one.cap", "one")
	stageCapsule(t, ctrl, "two.cap", "two")

	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules/clear", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if names := capsuleNames(t, ctrl); len(names) != 0 {
		t.Fatalf("capsules after clear = %v, want none", names)
	}
}

func TestFirmwarePanelListsStagedCapsules(t *testing.T) {
	r, ctrl := firmwareRouter(t)
	stageCapsule(t, ctrl, "bios-1.2.cap", "0123456789")

	req := httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "bios-1.2.cap") {
		t.Error("panel does not list the staged capsule")
	}
	if !strings.Contains(body, "10 B") {
		t.Error("panel does not show the staged capsule's size")
	}
	// The row must carry its own delete control, or the only way to un-stage
	// one capsule is to clear all of them.
	if !strings.Contains(body, "/ui/settings/firmware/capsules/bios-1.2.cap") {
		t.Error("staged capsule row has no delete control")
	}
	// An operator who is not told the host applies these at ITS next boot
	// stages a capsule, sees nothing happen, and stages it again.
	if !strings.Contains(body, "next boot") {
		t.Error("panel never says the capsules apply at the host's next boot")
	}
	if !strings.Contains(body, "deletes each one it applies") {
		t.Error("panel never says a capsule leaving the list is how an operator confirms it was applied")
	}
}

func TestFirmwarePanelEmptyState(t *testing.T) {
	r, _ := firmwareRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing queued") {
		t.Error("panel has no empty state when nothing is staged")
	}
	// Clear all is meaningless with an empty queue and must not offer itself.
	if strings.Contains(body, "/ui/settings/firmware/capsules/clear") {
		t.Error("empty queue still offers Clear all")
	}
}

// The volume readout is the difference between "no capsules queued" and "the
// BMC never managed to create the volume", which look identical otherwise.
func TestFirmwarePanelReportsVolumeState(t *testing.T) {
	r, ctrl := firmwareRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, "Not created") {
		t.Error("panel does not report that the capsule volume does not exist yet")
	}

	// Staging creates the volume; the panel must now say so.
	stageCapsule(t, ctrl, "one.cap", "one")

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil))
	body := w.Body.String()
	if !strings.Contains(body, "256.0 MB") {
		t.Errorf("panel does not report the capsule volume size; body: %s", body)
	}
	if !strings.Contains(body, `\EFI\UpdateCapsule`) && !strings.Contains(body, `\\EFI\\UpdateCapsule`) {
		t.Error("panel does not name the directory the host firmware scans")
	}
}

func TestCapsuleFetchRejectsNonHTTPURL(t *testing.T) {
	r, _ := firmwareRouter(t)

	form := strings.NewReader("url=" + "file:///etc/passwd")
	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules/fetch", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "error") {
		t.Errorf("HX-Trigger = %q, want an error toast rejecting the scheme", trigger)
	}
	if firmwareFetchBusy() {
		t.Error("a rejected URL must not latch the fetch tracker")
	}
}

// A second stage while one is running is refused rather than queued, matching
// the 409 POST /api/firmware/capsules already returns.
func TestCapsuleFetchRefusesASecondStage(t *testing.T) {
	r, _ := firmwareRouter(t)

	// Latch the tracker directly rather than starting a real download: the
	// controller's own latch is a file in /tmp shared by every test binary on
	// this machine, and holding it would break pkg/firmware's fetch tests.
	firmwareFetchStart("already-running.cap")
	t.Cleanup(func() { firmwareFetchFinish(nil); firmwareFetchClear() })

	form := strings.NewReader("url=https://example.invalid/host.cap")
	req := httptest.NewRequest(http.MethodPost, "/ui/settings/firmware/capsules/fetch", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for a second concurrent stage", w.Code)
	}
	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "already") {
		t.Errorf("HX-Trigger = %q, want a toast saying a capsule is already being staged", trigger)
	}
}

// While a stage is running the controls must be disabled: a second upload is
// refused by the handler anyway, and a form that submits into a 409 reads as a
// broken panel.
func TestFirmwarePanelDisablesControlsWhileStaging(t *testing.T) {
	r, _ := firmwareRouter(t)

	firmwareFetchStart("in-flight.cap")
	t.Cleanup(func() { firmwareFetchFinish(nil); firmwareFetchClear() })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil))

	body := w.Body.String()
	if !strings.Contains(body, "in-flight.cap") {
		t.Error("panel does not name the capsule being staged")
	}
	// The poller is what eventually swaps the finished panel back in.
	if !strings.Contains(body, "/ui/settings/firmware/status") {
		t.Error("panel does not poll the status route while a stage is running")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("panel leaves its controls enabled while a stage is running")
	}
}

// The poller answers with the settled panel once the fetch finishes, and
// reports the outcome exactly once.
func TestFirmwareStatusReportsFetchOutcome(t *testing.T) {
	r, _ := firmwareRouter(t)

	firmwareFetchStart("done.cap")
	firmwareFetchFinish(nil)
	t.Cleanup(firmwareFetchClear)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/settings/firmware/status", nil))

	if trigger := w.Header().Get("HX-Trigger"); !strings.Contains(trigger, "success") {
		t.Errorf("HX-Trigger = %q, want a success toast once the fetch settles", trigger)
	}
	if body := w.Body.String(); strings.Contains(body, "/ui/settings/firmware/status") {
		t.Error("the settled panel must stop polling")
	}

	// A second poll (a request already in flight when the first landed) must
	// not raise the toast again.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/settings/firmware/status", nil))
	if trigger := w.Header().Get("HX-Trigger"); strings.Contains(trigger, "success") {
		t.Error("the fetch outcome was toasted twice")
	}
}

// os.TempDir() must be untouched by the read paths too — GetStatus opens the
// volume image on every render.
func TestFirmwarePanelDoesNotTouchTempDir(t *testing.T) {
	r, ctrl := firmwareRouter(t)
	t.Setenv("TMPDIR", t.TempDir())
	stageCapsule(t, ctrl, "one.cap", "one")

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ui/settings/firmware", nil))

	if names := tempDirNames(t); len(names) != 0 {
		t.Errorf("rendering the panel left files in os.TempDir(): %v", names)
	}
	if _, err := os.Stat(os.TempDir()); err != nil {
		t.Fatalf("stat temp dir: %v", err)
	}
}
