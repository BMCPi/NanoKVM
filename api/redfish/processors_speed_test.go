package redfish

// Tests for the Processor speed-cap lane: an operator PATCHes the standard
// SpeedLimitMHz/SpeedLocked properties onto the member, the host's
// inventory re-POST must not wipe them, and the host's Processor feature
// driver refreshes them through the same PATCH route.

import (
	"encoding/json"
	"net/http"
	"testing"
)

const testCPUBody = `{"Socket": "CPU0", "Model": "BCM2712", "MaxSpeedMHz": 2400,
	"@odata.type": "#Processor.v1_14_0.Processor"}`

func postCPU(t *testing.T, body string) {
	t.Helper()
	r := hostRouter()
	if w := do(r, http.MethodPost, "/redfish/v1/Systems/1/Processors", hostIP(t), body, nil); w.Code != http.StatusCreated {
		t.Fatalf("POST processor = %d: %s", w.Code, w.Body.String())
	}
}

func TestProcessorOperatorSpeedPatch(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postCPU(t, testCPUBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/Processors/CPU0", lanIP,
		`{"SpeedLimitMHz": 2800, "SpeedLocked": true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("operator PATCH = %d: %s", w.Code, w.Body.String())
	}

	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	locked, isBool := m["SpeedLocked"].(bool)
	if m["SpeedLimitMHz"] != float64(2800) || !isBool || !locked {
		t.Errorf("merged member = %v; want the staged pair", m)
	}
	// The inventory the host reported survives the merge.
	if m["Model"] != "BCM2712" {
		t.Errorf("Model = %v; report fields must survive an operator PATCH", m["Model"])
	}
}

// The host re-POSTs its SMBIOS inventory every boot, replacing the member —
// but never carries the operator-writable pair, which must be carried
// forward or a value staged between boots is wiped before the firmware's
// consume pass can read it.
func TestProcessorRepostPreservesStagedSpeed(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postCPU(t, testCPUBody)

	if w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/Processors/CPU0", lanIP,
		`{"SpeedLimitMHz": 2800, "SpeedLocked": false}`, nil); w.Code != http.StatusOK {
		t.Fatalf("stage PATCH = %d", w.Code)
	}

	// Next boot's re-POST: same Socket key, no speed properties.
	postCPU(t, testCPUBody)

	stored, ok := hostCollectionGet(processorsOf, "CPU0")
	if !ok {
		t.Fatal("member gone after re-POST")
	}
	locked, isBool := stored["SpeedLocked"].(bool)
	if stored["SpeedLimitMHz"] != float64(2800) || !isBool || locked {
		t.Errorf("staged pair wiped by inventory re-POST: %v", stored)
	}

	// A re-POST that DOES carry the pair (a future firmware echoing its
	// applied state) wins over the carried-forward values.
	postCPU(t, `{"Socket": "CPU0", "Model": "BCM2712", "SpeedLimitMHz": 3000, "SpeedLocked": true}`)
	stored, _ = hostCollectionGet(processorsOf, "CPU0")
	if stored["SpeedLimitMHz"] != float64(3000) {
		t.Errorf("host-carried value did not win: %v", stored["SpeedLimitMHz"])
	}
}

func TestProcessorPatchRejectsIdentityOnly(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postCPU(t, testCPUBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/Processors/CPU0", lanIP,
		`{"Id": "CPU9", "@odata.id": "/x"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("identity-only PATCH = %d, want 400", w.Code)
	}
	if stored, _ := hostCollectionGet(processorsOf, "CPU0"); stored["Id"] == "CPU9" {
		t.Error("identity key leaked into the stored member")
	}
}
