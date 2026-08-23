package redfish

// schema_version_test.go pins the schema versions this BMC advertises.
//
// These are not free-floating version numbers: the managed host runs EDK2's
// RedfishClientPkg, and each of its feature drivers is compiled against one
// exact schema version, which is also the version its HII questions are
// tagged with (x-UEFI-redfish-<Schema>.<version>). The RPi5 firmware builds
// Features/ComputerSystem/v1_5_0 and Features/Bios/v1_0_9.
//
// Advertising a different version is not an outright failure — the client
// rewrites @odata.type to its own when PcdRedfishCompatibleSchemaSupport is
// set, which it is by default — but that is a fallback path, and a mismatch
// is invisible from the BMC side. A test is the only place it shows up.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// hostSchemaVersions is the version each RedfishClientPkg feature driver in
// the RPi5 firmware is built against. Spelled out literally rather than
// referring to the constants so that changing a constant without meaning to
// fails here instead of silently agreeing with itself.
var hostSchemaVersions = map[string]string{
	"ComputerSystem":    "#ComputerSystem.v1_5_0.ComputerSystem",
	"Bios":              "#Bios.v1_0_9.Bios",
	"AttributeRegistry": "#AttributeRegistry.v1_3_6.AttributeRegistry",
	"BootOption":        "#BootOption.v1_0_4.BootOption",
	"SecureBoot":        "#SecureBoot.v1_1_0.SecureBoot",
	"Memory":            "#Memory.v1_7_1.Memory",
}

// TestSchemaVersionConstantsMatchHostClient pins the table itself. The
// constants are what every handler renders with, so checking them covers the
// resources this BMC serves on the host's behalf (memory modules, boot
// options, the registry) without standing up a host report for each.
func TestSchemaVersionConstantsMatchHostClient(t *testing.T) {
	for schema, want := range map[string]string{
		"ComputerSystem":    odataTypeComputerSystem,
		"Bios":              odataTypeBios,
		"AttributeRegistry": odataTypeAttributeRegistry,
		"BootOption":        odataTypeBootOption,
		"SecureBoot":        odataTypeSecureBoot,
		"Memory":            odataTypeMemory,
	} {
		if want != hostSchemaVersions[schema] {
			t.Errorf("%s constant = %q, want %q — the host client is built against that version",
				schema, want, hostSchemaVersions[schema])
		}
	}
}

func TestAdvertisedSchemaVersionsMatchHostClient(t *testing.T) {
	resetHostState(t)
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET(systemPath, svc.GetSystem)
	r.GET(biosPath, svc.GetBios)
	r.GET(biosSettingsPath, svc.GetBiosSettings)

	get := func(path string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return body
	}

	for path, schema := range map[string]string{
		systemPath: "ComputerSystem",
		biosPath:   "Bios",
	} {
		want := hostSchemaVersions[schema]
		if got, _ := get(path)["@odata.type"].(string); got != want {
			t.Errorf("GET %s @odata.type = %q, want %q", path, got, want)
		}
	}

	// The settings object is consumed by the same Bios driver, so it has to
	// carry the same version as the resource it hangs off.
	if got, _ := get(biosSettingsPath)["@odata.type"].(string); got != hostSchemaVersions["Bios"] {
		t.Errorf("GET %s @odata.type = %q, want %q", biosSettingsPath, got, hostSchemaVersions["Bios"])
	}
}

// computerSystemV150Properties is the ComputerSystem v1.5.0 property set, as
// spelled by the client's own generated structure
// (RedfishClientPkg/ConverterLib/include/Redfish_ComputerSystem_v1_5_0_CS.h).
// A property outside this set is one the declared version does not define.
var computerSystemV150Properties = map[string]bool{
	"@odata.context": true, "@odata.etag": true, "@odata.id": true, "@odata.type": true,
	"Actions": true, "AssetTag": true, "Bios": true, "BiosVersion": true, "Boot": true,
	"Description": true, "EthernetInterfaces": true, "HostName": true,
	"HostWatchdogTimer": true, "HostedServices": true, "HostingRoles": true, "Id": true,
	"IndicatorLED": true, "Links": true, "LogServices": true, "Manufacturer": true,
	"Memory": true, "MemoryDomains": true, "MemorySummary": true, "Model": true,
	"Name": true, "NetworkInterfaces": true, "Oem": true, "PCIeDevices": true,
	"PCIeFunctions": true, "PartNumber": true, "PowerState": true, "Processors": true,
	"ProcessorSummary": true, "Redundancy": true, "SKU": true, "SecureBoot": true,
	"SerialNumber": true, "SimpleStorage": true, "Status": true, "Storage": true,
	"SubModel": true, "SystemType": true, "TrustedModules": true, "UUID": true,
}

// A document has to conform to the version it declares. This catches the case
// where a property from a newer ComputerSystem is emitted under the v1.5.0
// type — the client silently drops what its structure has no field for, so
// the BMC would be publishing something no one reads.
func TestComputerSystemConformsToDeclaredVersion(t *testing.T) {
	resetHostState(t)
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()
	r.GET(systemPath, svc.GetSystem)
	r.PATCH(systemPath, svc.PatchSystem)

	// Report host state first: several properties are omitted until the host
	// has reported, and an empty document would conform vacuously.
	pw := do(r, http.MethodPatch, systemPath, hostIP(t),
		`{"Manufacturer":"Raspberry Pi Foundation","Model":"Raspberry Pi 5 Model B",`+
			`"SerialNumber":"S1","UUID":"11111111-2222-3333-4444-555555555555",`+
			`"BiosVersion":"202608","BootProgress":{"LastState":"OSRunning"}}`,
		map[string]string{"Content-Type": "application/json"})
	if pw.Code != http.StatusOK {
		t.Fatalf("host PATCH = %d, want 200: %s", pw.Code, pw.Body.String())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, systemPath, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var extra []string
	for k := range body {
		if !computerSystemV150Properties[k] && !strings.Contains(k, "@Redfish.") {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		t.Errorf("ComputerSystem declares v1_5_0 but emits properties it does not define: %v", extra)
	}
}
