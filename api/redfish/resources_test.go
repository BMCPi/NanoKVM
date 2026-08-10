package redfish

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// decodeBody serves one handler and returns the body as a generic map.
func decodeBody(t *testing.T, h gin.HandlerFunc) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	return got
}

// The service root must be readable by gofish's own ServiceRoot schema, and
// its navigation links must survive the round-trip as @odata.id objects.
// This is the contract the migration exists to satisfy.
func TestServiceRootIsParsableByGofish(t *testing.T) {
	svc := NewService()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/root", svc.GetServiceRoot)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/root", nil))

	// gofish.Service is gofish's ServiceRoot type.
	var root gofish.Service
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatalf("gofish cannot parse our ServiceRoot: %v\nbody: %s", err, w.Body)
	}

	if root.ODataID != schemas.DefaultServiceRoot {
		t.Errorf("@odata.id = %q, want %q", root.ODataID, schemas.DefaultServiceRoot)
	}
	if root.ID != "ServiceRoot" {
		t.Errorf("Id = %q, want ServiceRoot", root.ID)
	}

	// gofish resolves these through its Link unmarshaler; a bare-string or
	// missing link would leave them empty and break Login/discovery.
	body := decodeBody(t, svc.GetServiceRoot)
	for _, prop := range []string{"Systems", "Managers", "Chassis", "SessionService", "UpdateService"} {
		obj, ok := body[prop].(map[string]any)
		if !ok {
			t.Errorf("%s is %T, want an {\"@odata.id\": ...} object", prop, body[prop])
			continue
		}
		if _, ok := obj["@odata.id"].(string); !ok {
			t.Errorf("%s missing @odata.id", prop)
		}
	}

	// Links.Sessions is what gofish POSTs to on Login.
	links, ok := body["Links"].(map[string]any)
	if !ok {
		t.Fatal("Links missing")
	}
	sessions, ok := links["Sessions"].(map[string]any)
	if !ok {
		t.Fatal("Links.Sessions missing")
	}
	if got := sessions["@odata.id"]; got != sessionsPath {
		t.Errorf("Links.Sessions = %v, want %s", got, sessionsPath)
	}
}

// Redfish enums have no valid empty member. Emitting "PowerState": "" — which
// is what marshalling a gofish struct would produce — is schema-invalid, so
// unset properties must be omitted entirely.
func TestSystemOmitsEmptyEnums(t *testing.T) {
	body := decodeBody(t, NewService().GetSystem)

	for _, prop := range []string{
		"SystemType", "PowerState", "Manufacturer", "Model", "SKU",
		"SerialNumber", "UUID", "BiosVersion", "SubModel", "IndicatorLED",
	} {
		v, present := body[prop]
		if !present {
			continue // omitted is fine
		}
		if s, ok := v.(string); ok && s == "" {
			t.Errorf("%s present but empty — omit it instead", prop)
		}
	}

	// Internal gofish bookkeeping must never appear.
	for _, leak := range []string{"RawData", "SettingsApplyTimes"} {
		if _, present := body[leak]; present {
			t.Errorf("%s leaked into the response", leak)
		}
	}
}

// The Bios navigation property is the one gofish literally cannot express
// (ComputerSystem.bios is unexported), so pin its wire form.
func TestSystemBiosLinkIsODataRef(t *testing.T) {
	body := decodeBody(t, NewService().GetSystem)

	bios, ok := body["Bios"].(map[string]any)
	if !ok {
		t.Fatalf("Bios is %T, want an object", body["Bios"])
	}
	if got := bios["@odata.id"]; got != biosPath {
		t.Errorf("Bios.@odata.id = %v, want %s", got, biosPath)
	}
}

// Boot must always advertise its allowable values so clients know what to
// PATCH, and the enums must be the gofish-typed ones.
func TestSystemBootBlock(t *testing.T) {
	body := decodeBody(t, NewService().GetSystem)

	boot, ok := body["Boot"].(map[string]any)
	if !ok {
		t.Fatal("Boot missing")
	}
	if boot["BootSourceOverrideTarget"] != string(schemas.NoneBootSource) {
		t.Errorf("target = %v, want None", boot["BootSourceOverrideTarget"])
	}
	if boot["BootSourceOverrideEnabled"] != string(schemas.DisabledBootSourceOverrideEnabled) {
		t.Errorf("enabled = %v, want Disabled", boot["BootSourceOverrideEnabled"])
	}
	if boot["BootSourceOverrideMode"] != string(schemas.UEFIBootSourceOverrideMode) {
		t.Errorf("mode = %v, want UEFI", boot["BootSourceOverrideMode"])
	}
	allow, ok := boot["BootSourceOverrideTarget@Redfish.AllowableValues"].([]any)
	if !ok || len(allow) != len(supportedBootSources) {
		t.Fatalf("AllowableValues = %v, want %d entries", allow, len(supportedBootSources))
	}
}

// A collection's count must match its members — a hand-written map lets these
// drift, which is why newCollection derives one from the other.
func TestCollectionCountMatchesMembers(t *testing.T) {
	for name, h := range map[string]gin.HandlerFunc{
		"Systems":           NewService().GetSystemCollection,
		"Managers":          NewService().GetManagerCollection,
		"Sessions":          NewService().GetSessionCollection,
		"FirmwareInventory": NewService().GetFirmwareInventoryCollection,
	} {
		t.Run(name, func(t *testing.T) {
			body := decodeBody(t, h)
			members, ok := body["Members"].([]any)
			if !ok {
				t.Fatalf("Members is %T, want an array", body["Members"])
			}
			count, ok := body["Members@odata.count"].(float64)
			if !ok {
				t.Fatal("Members@odata.count missing")
			}
			if int(count) != len(members) {
				t.Errorf("count = %d but %d members", int(count), len(members))
			}
			for i, m := range members {
				obj, ok := m.(map[string]any)
				if !ok {
					t.Errorf("member %d is %T, want an object", i, m)
					continue
				}
				if _, ok := obj["@odata.id"].(string); !ok {
					t.Errorf("member %d missing @odata.id", i)
				}
			}
		})
	}
}

// gofish must be able to read the Manager, including the Dell OEM block the
// terraform provider depends on.
func TestManagerIsParsableByGofish(t *testing.T) {
	svc := NewService()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/m", svc.GetManager)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/m", nil))

	var mgr schemas.Manager
	if err := json.Unmarshal(w.Body.Bytes(), &mgr); err != nil {
		t.Fatalf("gofish cannot parse our Manager: %v\nbody: %s", err, w.Body)
	}
	if mgr.ManagerType != schemas.BMCManagerType {
		t.Errorf("ManagerType = %q, want BMC", mgr.ManagerType)
	}

	// Oem and Actions must be present-but-empty rather than absent, or
	// gofish's dell.Manager() wrapper fails with "unexpected end of JSON
	// input".
	body := decodeBody(t, svc.GetManager)
	for _, prop := range []string{"Oem", "Actions"} {
		if _, present := body[prop]; !present {
			t.Errorf("%s absent; dell.Manager() needs it present even when empty", prop)
		}
	}
}

// Reset must advertise exactly the types ResetSystem can service.
func TestSystemResetAllowableValues(t *testing.T) {
	body := decodeBody(t, NewService().GetSystem)

	actions, ok := body["Actions"].(map[string]any)
	if !ok {
		t.Fatal("Actions missing")
	}
	reset, ok := actions["#ComputerSystem.Reset"].(map[string]any)
	if !ok {
		t.Fatal("#ComputerSystem.Reset missing")
	}
	if reset["target"] != systemResetPath {
		t.Errorf("target = %v, want %s", reset["target"], systemResetPath)
	}
	allow, ok := reset["ResetType@Redfish.AllowableValues"].([]any)
	if !ok {
		t.Fatal("ResetType@Redfish.AllowableValues missing")
	}
	if len(allow) != len(supportedResetTypes) {
		t.Errorf("advertises %d reset types, supportedResetTypes has %d",
			len(allow), len(supportedResetTypes))
	}
	// Every advertised value must actually be accepted.
	for _, v := range allow {
		if !resetTypeSupported(schemas.ResetType(v.(string))) {
			t.Errorf("advertises %q but resetTypeSupported rejects it", v)
		}
	}
}

// The base /redfish document must point at the trailing-slash root.
func TestRedfishBasePointsAtDefaultServiceRoot(t *testing.T) {
	body := decodeBody(t, NewService().GetRedfishBase)
	if body["v1"] != schemas.DefaultServiceRoot {
		t.Errorf("v1 = %v, want %q", body["v1"], schemas.DefaultServiceRoot)
	}
}

// Every @odata.id we emit must sit under the service root.
func TestODataIDsAreRooted(t *testing.T) {
	svc := NewService()
	for name, h := range map[string]gin.HandlerFunc{
		"ServiceRoot":       svc.GetServiceRoot,
		"SystemCollection":  svc.GetSystemCollection,
		"System":            svc.GetSystem,
		"ManagerCollection": svc.GetManagerCollection,
		"Manager":           svc.GetManager,
		"SessionService":    svc.GetSessionService,
		"UpdateService":     svc.GetUpdateService,
	} {
		t.Run(name, func(t *testing.T) {
			body := decodeBody(t, h)
			id, ok := body["@odata.id"].(string)
			if !ok {
				t.Fatal("@odata.id missing")
			}
			if !strings.HasPrefix(id, schemas.DefaultServiceRoot) {
				t.Errorf("@odata.id = %q, want it under %q", id, schemas.DefaultServiceRoot)
			}
			if body["@odata.type"] == "" {
				t.Error("@odata.type is empty")
			}
		})
	}
}
