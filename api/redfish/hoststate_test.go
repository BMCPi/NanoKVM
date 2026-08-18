package redfish

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// TestMain points the persisted host state at a scratch file so tests never
// touch /var/lib/nanokvm — and so SetBootOverride's immediate flush cannot
// fail on a developer machine.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bmc-hoststate-*")
	if err != nil {
		panic(err)
	}
	hostStatePath = filepath.Join(dir, "bmc_state.json")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// resetHostState puts the singleton back to its pristine shape between tests.
func resetHostState(t *testing.T) {
	t.Helper()
	reset := func() {
		host.mu.Lock()
		host.Boot = HostBootOverride{Target: "None", Enabled: "Disabled"}
		host.Host = HostReport{}
		host.BootOptions = map[string]map[string]any{}
		host.Memory = map[string]map[string]any{}
		host.Processors = map[string]map[string]any{}
		host.Drives = map[string]map[string]any{}
		host.BiosAttributes = map[string]any{}
		host.BiosPending = map[string]any{}
		host.BiosRegistry = nil
		host.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// hostIP derives an address inside the configured RHI subnet, so the tests
// track whatever CIDR the config carries instead of hard-coding one.
func hostIP(t *testing.T) string {
	t.Helper()
	cidr := config.GetInstance().Network.RHI.Address
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("config RHI address %q unparseable: %v", cidr, err)
	}
	return ip.String()
}

// hostRouter registers the full route table without auth interference (the
// trust boundary under test is hostWritable, not CheckAuth).
func hostRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET("/redfish/v1/Systems/1", svc.GetSystem)
	r.PATCH("/redfish/v1/Systems/1", svc.PatchSystem)
	r.GET("/redfish/v1/Systems/1/BootOptions", svc.GetBootOptionCollection)
	r.POST("/redfish/v1/Systems/1/BootOptions", svc.PostBootOption)
	r.GET("/redfish/v1/Systems/1/BootOptions/:option", svc.GetBootOption)
	r.PATCH("/redfish/v1/Systems/1/BootOptions/:option", svc.PatchBootOption)
	r.DELETE("/redfish/v1/Systems/1/BootOptions/:option", svc.DeleteBootOption)
	r.GET("/redfish/v1/Systems/1/Memory", svc.GetMemoryCollection)
	r.POST("/redfish/v1/Systems/1/Memory", svc.PostMemoryModule)
	r.GET("/redfish/v1/Systems/1/Memory/:module", svc.GetMemoryModule)
	r.GET("/redfish/v1/Systems/1/Processors", svc.GetProcessorCollection)
	r.POST("/redfish/v1/Systems/1/Processors", svc.PostProcessor)
	r.GET("/redfish/v1/Systems/1/Processors/:processor", svc.GetProcessor)
	r.PATCH("/redfish/v1/Systems/1/Processors/:processor", svc.PatchProcessor)
	r.DELETE("/redfish/v1/Systems/1/Processors/:processor", svc.DeleteProcessor)
	r.GET("/redfish/v1/Systems/1/Storage/:storage", svc.GetStorage)
	r.POST("/redfish/v1/Systems/1/Storage/:storage/Drives", svc.PostHostDrive)
	r.GET("/redfish/v1/Systems/1/Storage/:storage/Drives/:drive", svc.GetDrive)
	r.GET("/redfish/v1/Systems/1/Bios", svc.GetBios)
	r.PATCH("/redfish/v1/Systems/1/Bios", svc.PatchBios)
	r.GET("/redfish/v1/Systems/1/Bios/Settings", svc.GetBiosSettings)
	r.PATCH("/redfish/v1/Systems/1/Bios/Settings", svc.PatchBiosSettings)
	r.GET("/redfish/v1/Systems/1/SecureBoot", svc.GetSecureBoot)
	r.PATCH("/redfish/v1/Systems/1/SecureBoot", svc.PatchSecureBoot)
	return r
}

// do performs a request from the given source address.
func do(r *gin.Engine, method, path, from, body string, header map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.RemoteAddr = from + ":40000"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const lanIP = "192.0.2.10"

// Host-owned resources must reject writes from anywhere but the host
// interface — a LAN client claiming the host has hardware it does not have
// would be repeated back as fact.
func TestHostWritesRejectedFromLAN(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/redfish/v1/Systems/1/BootOptions"},
		{http.MethodPost, "/redfish/v1/Systems/1/Memory"},
		{http.MethodPost, "/redfish/v1/Systems/1/Processors"},
		{http.MethodPatch, "/redfish/v1/Systems/1/Processors/CPU1"},
		{http.MethodDelete, "/redfish/v1/Systems/1/Processors/CPU1"},
		{http.MethodPost, "/redfish/v1/Systems/1/Storage/1/Drives"},
		{http.MethodPatch, "/redfish/v1/Systems/1/Bios"},
		{http.MethodPatch, "/redfish/v1/Systems/1/SecureBoot"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(r, tc.method, tc.path, lanIP, `{"Id":"x"}`, nil)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s from LAN = %d, want 403", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestBootOptionLifecycle(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	// POST keyed by BootOptionReference.
	w := do(r, http.MethodPost, "/redfish/v1/Systems/1/BootOptions", from,
		`{"BootOptionReference":"Boot0001","DisplayName":"UEFI PXE","BootOptionEnabled":true}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want exactly application/json (EDK2 rejects a charset suffix)", got)
	}
	if loc := w.Header().Get("Location"); loc != bootOptionsPath+"/Boot0001" {
		t.Errorf("Location = %q", loc)
	}

	// Collection lists it.
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/BootOptions", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET collection = %d", w.Code)
	}
	var coll struct {
		Count   int `json:"Members@odata.count"`
		Members []struct {
			ID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &coll); err != nil {
		t.Fatalf("collection unmarshal: %v", err)
	}
	if coll.Count != 1 || len(coll.Members) != 1 || coll.Members[0].ID != bootOptionsPath+"/Boot0001" {
		t.Fatalf("collection = %+v", coll)
	}

	// Member GET carries an ETag and honours If-None-Match.
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/BootOptions/Boot0001", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET member = %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("member GET has no ETag")
	}
	var member map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &member); err != nil {
		t.Fatalf("member unmarshal: %v", err)
	}
	if member["@odata.etag"] != etag {
		t.Errorf("@odata.etag = %v, header = %q", member["@odata.etag"], etag)
	}
	if member["DisplayName"] != "UEFI PXE" {
		t.Errorf("stored report lost DisplayName: %v", member)
	}

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/BootOptions/Boot0001", lanIP, "",
		map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Errorf("If-None-Match matched but status = %d, want 304", w.Code)
	}

	// PATCH with a stale If-Match must 412; with the right one it merges.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/BootOptions/Boot0001", from,
		`{"BootOptionEnabled":false}`, map[string]string{"If-Match": `W/"stale"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match = %d, want 412", w.Code)
	}
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/BootOptions/Boot0001", from,
		`{"BootOptionEnabled":false}`, map[string]string{"If-Match": etag})
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}
	var patched map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	enabled, _ := patched["BootOptionEnabled"].(bool)
	if enabled || patched["DisplayName"] != "UEFI PXE" {
		t.Errorf("PATCH did not merge: %v", patched)
	}

	// DELETE removes it.
	if w = do(r, http.MethodDelete, "/redfish/v1/Systems/1/BootOptions/Boot0001", from, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", w.Code)
	}
	if w = do(r, http.MethodGet, "/redfish/v1/Systems/1/BootOptions/Boot0001", lanIP, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", w.Code)
	}
}

// Memory and drive reports flow into their collections and the Storage "1"
// subsystem links what the host reported.
func TestHostReportedMemoryAndDrives(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Memory", from,
		`{"Id":"DIMM0","CapacityMiB":16384,"MemoryDeviceType":"LPDDR4_SDRAM"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST memory = %d, body %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Memory/DIMM0", lanIP, "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "16384") {
		t.Errorf("GET memory member = %d, body %s", w.Code, w.Body.String())
	}

	w = do(r, http.MethodPost, "/redfish/v1/Systems/1/Storage/1/Drives", from,
		`{"Id":"nvme0","Model":"Samsung SSD 990","CapacityBytes":1000204886016,"Protocol":"NVMe"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST drive = %d, body %s", w.Code, w.Body.String())
	}

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Storage/1", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET storage = %d", w.Code)
	}
	var storage struct {
		Drives []struct {
			ID string `json:"@odata.id"`
		} `json:"Drives"`
		Count int `json:"Drives@odata.count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &storage); err != nil {
		t.Fatalf("storage unmarshal: %v", err)
	}
	if storage.Count != 1 || storage.Drives[0].ID != drivesPath+"/nvme0" {
		t.Errorf("storage = %+v", storage)
	}

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Storage/1/Drives/nvme0", lanIP, "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Samsung SSD 990") {
		t.Errorf("GET drive = %d, body %s", w.Code, w.Body.String())
	}
}

// The Bios split: the host reports live attributes (replace), the operator
// stages pending ones, and the host clears the stage after applying.
func TestBiosReportAndStaging(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	// Host reports its live attributes.
	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/Bios", from,
		`{"Attributes":{"BootTimeout":5,"SdBoot":true}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("host PATCH Bios = %d, body %s", w.Code, w.Body.String())
	}

	// A later report replaces wholesale — the dropped key must vanish.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/Bios", from,
		`{"Attributes":{"BootTimeout":3}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("second host PATCH Bios = %d", w.Code)
	}
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios", lanIP, "", nil)
	var bios struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bios); err != nil {
		t.Fatalf("bios unmarshal: %v", err)
	}
	if _, stale := bios.Attributes["SdBoot"]; stale || bios.Attributes["BootTimeout"] != float64(3) {
		t.Errorf("Attributes = %v, want replace-not-merge", bios.Attributes)
	}

	// Operator stages from the LAN — the settings object is not host-owned.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/Bios/Settings", lanIP,
		`{"Attributes":{"BootTimeout":10}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("operator PATCH Bios/Settings = %d, body %s", w.Code, w.Body.String())
	}
	if pending := hostBiosPending(); pending["BootTimeout"] != float64(10) {
		t.Errorf("pending = %v", pending)
	}

	// The host clears the stage after applying.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/Bios/Settings", from,
		`{"Attributes":{}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("host clear PATCH = %d", w.Code)
	}
	if pending := hostBiosPending(); len(pending) != 0 {
		t.Errorf("pending not cleared: %v", pending)
	}
}

// PATCH /Systems/1 carries both directions: boot override from anywhere,
// identity reports only from the host interface.
func TestPatchSystemDirections(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	// Identity report from the LAN is refused.
	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1", lanIP,
		`{"Model":"forged"}`, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("LAN identity report = %d, want 403", w.Code)
	}

	// From the host it lands, and the response is the full system resource.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1", from,
		`{"BiosVersion":"2026.07","Model":"Raspberry Pi 5","SerialNumber":"S123","UUID":"11111111-2222-3333-4444-555555555555","BootProgress":{"LastState":"OSRunning"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("host PATCH = %d, body %s", w.Code, w.Body.String())
	}
	var sys struct {
		BiosVersion  string `json:"BiosVersion"`
		Model        string `json:"Model"`
		BootProgress *struct {
			LastState string `json:"LastState"`
		} `json:"BootProgress"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sys); err != nil {
		t.Fatalf("system unmarshal: %v", err)
	}
	if sys.BiosVersion != "2026.07" || sys.Model != "Raspberry Pi 5" ||
		sys.BootProgress == nil || sys.BootProgress.LastState != "OSRunning" {
		t.Errorf("system = %+v", sys)
	}

	// Boot override from the LAN is the operator path and must work; the
	// host reads the staged override out of the response.
	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1", lanIP,
		`{"Boot":{"BootSourceOverrideTarget":"Pxe","BootSourceOverrideEnabled":"Once"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("operator boot PATCH = %d, body %s", w.Code, w.Body.String())
	}
	if ov := BootOverride(); ov.Target != "Pxe" || ov.Enabled != "Once" {
		t.Errorf("override = %+v", ov)
	}
}

// SecureBoot is host-owned state.
func TestSecureBootHostReport(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/SecureBoot", from,
		`{"SecureBootCurrentBoot":"Enabled","SecureBootEnable":true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("host PATCH SecureBoot = %d, body %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/SecureBoot", lanIP, "", nil)
	var sb map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &sb); err != nil {
		t.Fatalf("secureboot unmarshal: %v", err)
	}
	sbEnable, _ := sb["SecureBootEnable"].(bool)
	if sb["SecureBootCurrentBoot"] != "Enabled" || !sbEnable {
		t.Errorf("SecureBoot = %v", sb)
	}
}

// An operator instruction (the boot override) must hit the disk immediately,
// not on the debounce timer — power can drop before a debounce fires.
func TestBootOverridePersistsImmediately(t *testing.T) {
	resetHostState(t)

	SetBootOverride("Cd", "Continuous")
	t.Cleanup(clearBootOverride)

	data, err := os.ReadFile(hostStatePath)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var persisted struct {
		Boot HostBootOverride `json:"boot"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("state file unreadable: %v", err)
	}
	if persisted.Boot.Target != "Cd" || persisted.Boot.Enabled != "Continuous" {
		t.Errorf("persisted boot = %+v", persisted.Boot)
	}
}

// A host report persists too (debounced), and LoadHostState restores it.
func TestHostStateRoundTrip(t *testing.T) {
	resetHostState(t)

	hostCollectionPut(bootOptionsOf, "Boot0000", map[string]any{"DisplayName": "SD"})
	updateHostReported(func(h *HostReport) { h.Model = "roundtrip" })
	hostStateFlush()

	// Wipe the in-memory state, then restore from disk.
	host.mu.Lock()
	host.BootOptions = map[string]map[string]any{}
	host.Host = HostReport{}
	host.mu.Unlock()

	LoadHostState()

	if got, ok := hostCollectionGet(bootOptionsOf, "Boot0000"); !ok || got["DisplayName"] != "SD" {
		t.Errorf("BootOptions not restored: %v (ok=%v)", got, ok)
	}
	reported, _ := HostReported()
	if reported.Model != "roundtrip" {
		t.Errorf("host report not restored: %+v", reported)
	}
	if reported.ReportedAt.IsZero() || time.Since(reported.ReportedAt) > time.Minute {
		t.Errorf("ReportedAt not preserved: %v", reported.ReportedAt)
	}
}

// The EDK2 RedfishClient derives the registry URI from the Bios resource's
// AttributeRegistry property and PUTs its generated document there. The name
// must be advertised before anything is published, the wildcard must accept
// the derived name (and versioned variants), and /Registries must point at
// the same document.
func TestBiosAttributeRegistryEDK2Flow(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	r.GET("/redfish/v1/Systems/1/Bios/:registry", NewService(testDeps()).GetBiosAttributeRegistry)
	r.PUT("/redfish/v1/Systems/1/Bios/:registry", NewService(testDeps()).PutBiosAttributeRegistry)
	from := hostIP(t)

	// Advertised before publication, so the client can build the PUT URI.
	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios", lanIP, "", nil)
	var bios struct {
		AttributeRegistry string `json:"AttributeRegistry"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &bios)
	if bios.AttributeRegistry != "BiosAttributeRegistry" {
		t.Fatalf("AttributeRegistry = %q before publication", bios.AttributeRegistry)
	}

	// Unpublished GET serves the base skeleton (the client GETs before it
	// PUTs and a 404 would end its walk); a bogus sibling name is 404.
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("GET unpublished = %d, want 200 base resource", w.Code)
	}
	var base map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &base)
	if _, ok := base["RegistryEntries"]; !ok {
		t.Errorf("base registry lacks RegistryEntries: %s", w.Body.String())
	}
	if w = do(r, http.MethodPut, "/redfish/v1/Systems/1/Bios/NotARegistry", from, `{}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("PUT bogus name = %d, want 404", w.Code)
	}

	// Host publishes; a versioned name must resolve to the same document.
	doc := `{"Id":"BiosAttributeRegistry.v1_0_0","RegistryEntries":{"Attributes":[{"AttributeName":"BootOrder"}]}}`
	if w = do(r, http.MethodPut, "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry.v1_0_0", from, doc, nil); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body %s", w.Code, w.Body.String())
	}
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET after PUT = %d", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["Id"] != "BiosAttributeRegistry" || got["@odata.id"] != "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry" {
		t.Errorf("registry identity does not track the requested URI: %v %v", got["Id"], got["@odata.id"])
	}
	// The Bios resource now advertises the published document's Id.
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios", lanIP, "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &bios)
	if bios.AttributeRegistry != "BiosAttributeRegistry.v1_0_0" {
		t.Errorf("AttributeRegistry after publication = %q", bios.AttributeRegistry)
	}
}

// The attribute registry is the one Bios sub-resource an authenticated
// operator may publish from the LAN: it is a vocabulary document, not a claim
// about hardware, so hostWritable does not gate it. It must also reach the
// disk on the PUT rather than on the debounce timer, and survive a restart.
func TestBiosAttributeRegistryPutFromLANPersists(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	svc := NewService(testDeps())
	r.GET("/redfish/v1/Systems/1/Bios/:registry", svc.GetBiosAttributeRegistry)
	r.PUT("/redfish/v1/Systems/1/Bios/:registry", svc.PutBiosAttributeRegistry)

	doc := `{"RegistryVersion":"1.2.3","RegistryEntries":{"Attributes":[{"AttributeName":"SerialRedirect"}]}}`
	w := do(r, http.MethodPut, "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry", lanIP, doc, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT from LAN = %d, want 200; body %s", w.Code, w.Body.String())
	}

	// On disk already — no debounce window to lose it in.
	data, err := os.ReadFile(hostStatePath)
	if err != nil {
		t.Fatalf("state file not written on PUT: %v", err)
	}
	var persisted struct {
		BiosRegistry map[string]any `json:"bios_registry"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("state file unreadable: %v", err)
	}
	if persisted.BiosRegistry["RegistryVersion"] != "1.2.3" {
		t.Fatalf("persisted registry = %v", persisted.BiosRegistry)
	}

	// Survives a restart: drop the in-memory copy and reload from disk.
	host.mu.Lock()
	host.BiosRegistry = nil
	host.mu.Unlock()
	LoadHostState()

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Bios/BiosAttributeRegistry", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET after reload = %d", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["RegistryVersion"] != "1.2.3" {
		t.Errorf("registry not restored across restart: %v", got["RegistryVersion"])
	}
}

// The host's thermal driver GETs then PATCHes Chassis/1/Thermal on every
// boot; the GET must be a valid resource even before the first report, and
// writes are host-interface-only like every other host-owned resource.
func TestChassisThermalHostReport(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	svc := NewService(testDeps())
	r.GET("/redfish/v1/Chassis/1/Thermal", svc.GetChassisThermal)
	r.PATCH("/redfish/v1/Chassis/1/Thermal", svc.PatchChassisThermal)
	from := hostIP(t)

	w := do(r, http.MethodGet, "/redfish/v1/Chassis/1/Thermal", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET before report = %d, want 200 (a 404 ends the driver's walk)", w.Code)
	}

	if w = do(r, http.MethodPatch, "/redfish/v1/Chassis/1/Thermal", lanIP, `{"Temperatures":[]}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("PATCH from LAN = %d, want 403", w.Code)
	}

	body := `{"Temperatures":[{"Name":"SoC","ReadingCelsius":48}]}`
	if w = do(r, http.MethodPatch, "/redfish/v1/Chassis/1/Thermal", from, body, nil); w.Code != http.StatusOK {
		t.Fatalf("PATCH from host = %d, body %s", w.Code, w.Body.String())
	}

	w = do(r, http.MethodGet, "/redfish/v1/Chassis/1/Thermal", lanIP, "", nil)
	var got struct {
		Temperatures []map[string]any `json:"Temperatures"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Temperatures) != 1 || got.Temperatures[0]["Name"] != "SoC" {
		t.Errorf("thermal report not served back: %s", w.Body.String())
	}
}

// A keyless report re-POSTed every boot must update, not accumulate — the
// bug observed on hardware as Memory/DIMM0..DIMM42, all the same module.
func TestKeylessReportsDoNotAccumulate(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	body := `{"CapacityMiB":4096,"Manufacturer":"Micron","DeviceLocator":"SDRAM"}`
	for range 3 {
		if w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Memory", from, body, nil); w.Code != http.StatusCreated {
			t.Fatalf("POST = %d", w.Code)
		}
	}
	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Memory", lanIP, "", nil)
	var coll struct {
		Count   int `json:"Members@odata.count"`
		Members []struct {
			ID string `json:"@odata.id"`
		} `json:"Members"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &coll)
	if coll.Count != 1 {
		t.Fatalf("3 identical reports produced %d members: %+v", coll.Count, coll.Members)
	}
	// DeviceLocator is the natural key, so the id is meaningful too.
	if coll.Members[0].ID != "/redfish/v1/Systems/1/Memory/SDRAM" {
		t.Errorf("member id = %q, want the DeviceLocator key", coll.Members[0].ID)
	}
}

// Ghosts persisted by earlier builds collapse on load.
func TestLoadCollapsesPersistedDuplicates(t *testing.T) {
	resetHostState(t)
	host.mu.Lock()
	for i := range 5 {
		host.Memory[fmt.Sprintf("DIMM%d", i)] = map[string]any{"CapacityMiB": float64(4096)}
	}
	host.Memory["Other"] = map[string]any{"CapacityMiB": float64(8192)}
	host.mu.Unlock()
	hostStateFlush()

	host.mu.Lock()
	host.Memory = map[string]map[string]any{}
	host.mu.Unlock()
	LoadHostState()

	host.mu.RLock()
	defer host.mu.RUnlock()
	if len(host.Memory) != 2 {
		t.Fatalf("restored %d members, want 2 (DIMM0 + Other): %v", len(host.Memory), host.Memory)
	}
	if _, ok := host.Memory["DIMM0"]; !ok {
		t.Error("lowest id did not survive the collapse")
	}
}

// --- Processors --------------------------------------------------------------

// TestProcessorPlaceholderBeforeHostReports: the BMC knows this board is an
// aarch64 part without being told, so a read before the host has ever booted
// answers with that rather than an empty collection.
func TestProcessorPlaceholderBeforeHostReports(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("collection = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/redfish/v1/Systems/1/Processors/CPU1") {
		t.Errorf("collection does not list the placeholder: %s", w.Body.String())
	}

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU1", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("CPU1 = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ARM") {
		t.Errorf("placeholder lost its architecture: %s", w.Body.String())
	}
}

// TestProcessorCollectionLifecycle is the host-owned path: POST creates members,
// GET returns them as sent, PATCH merges, DELETE removes.
func TestProcessorCollectionLifecycle(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	body := `{"Id":"CPU0","Model":"Cortex-A76","Manufacturer":"Broadcom","TotalCores":4}`
	w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Processors", from, body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/redfish/v1/Systems/1/Processors/CPU0" {
		t.Errorf("Location = %q", loc)
	}

	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU0", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}
	for _, want := range []string{"Cortex-A76", "Broadcom"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("stored processor is missing %q: %s", want, w.Body.String())
		}
	}

	w = do(r, http.MethodPatch, "/redfish/v1/Systems/1/Processors/CPU0", from,
		`{"MaxSpeedMHz":2400}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200: %s", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, "2400") || !strings.Contains(b, "Cortex-A76") {
		t.Errorf("PATCH did not merge onto the stored member: %s", b)
	}

	if w := do(r, http.MethodDelete, "/redfish/v1/Systems/1/Processors/CPU0", from, "", nil); w.Code != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", w.Code)
	}
	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU0", lanIP, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", w.Code)
	}
}

// TestProcessorCollectionHoldsEveryReport is what makes this a collection
// rather than one node: a multi-socket host gets one member per socket.
func TestProcessorCollectionHoldsEveryReport(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	from := hostIP(t)

	for _, id := range []string{"CPU0", "CPU1", "CPU2"} {
		if w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Processors", from,
			`{"Id":"`+id+`","Model":"Cortex-A76"}`, nil); w.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d", id, w.Code)
		}
	}

	got := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors", lanIP, "", nil).Body.String()
	for _, id := range []string{"CPU0", "CPU1", "CPU2"} {
		if !strings.Contains(got, "/redfish/v1/Systems/1/Processors/"+id) {
			t.Errorf("collection is missing %s: %s", id, got)
		}
	}
}

// TestProcessorReportReplacesPlaceholder covers the handover: once the host has
// enumerated, its list is the collection, and the BMC's placeholder is gone —
// keeping it would report a socket the host did not find.
func TestProcessorReportReplacesPlaceholder(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	if w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Processors", hostIP(t),
		`{"Id":"CPU0","Model":"Cortex-A76"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", w.Code)
	}

	got := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors", lanIP, "", nil).Body.String()
	if strings.Contains(got, "Processors/CPU1") {
		t.Errorf("placeholder still listed after a host report: %s", got)
	}
	if w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU1", lanIP, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("placeholder CPU1 = %d after a host report, want 404", w.Code)
	}
}

// TestProcessorHostMayReportCPU1Itself: nothing stops the host from using the
// placeholder's id, and when it does its data is what gets served.
func TestProcessorHostMayReportCPU1Itself(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	if w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Processors", hostIP(t),
		`{"Id":"CPU1","Model":"Cortex-A76"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", w.Code)
	}

	got := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU1", lanIP, "", nil).Body.String()
	if !strings.Contains(got, "Cortex-A76") {
		t.Errorf("host report did not replace the placeholder at its own id: %s", got)
	}
}

// TestProcessorPatchUnknownIs404 guards the merge path against creating a
// member that was never POSTed.
func TestProcessorPatchUnknownIs404(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	if w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/Processors/NoSuch", hostIP(t),
		`{"Model":"x"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown = %d, want 404", w.Code)
	}
}
