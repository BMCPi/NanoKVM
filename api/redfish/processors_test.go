package redfish

// Tests for the pre-sync Processor/Processors placeholder: the BMC is
// board-agnostic, so what it serves before the host has ever reported must
// not claim an architecture it does not actually know.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestProcessorPlaceholderIsArchitectureNeutral covers the neutral-placeholder
// requirement in the board-agnostic design (§3): before any host report, CPU1
// exists (a single-socket system has a processor by construction) but claims
// no ProcessorArchitecture/InstructionSet, since those are host-specific
// facts this BMC no longer assumes (the old placeholder hardcoded ARM/A64).
func TestProcessorPlaceholderIsArchitectureNeutral(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU1", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET placeholder CPU1 = %d, body %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if _, ok := m["ProcessorArchitecture"]; ok {
		t.Errorf("placeholder claims ProcessorArchitecture = %v, want it absent pre-sync", m["ProcessorArchitecture"])
	}
	if _, ok := m["InstructionSet"]; ok {
		t.Errorf("placeholder claims InstructionSet = %v, want it absent pre-sync", m["InstructionSet"])
	}
	if m["ProcessorType"] != "CPU" {
		t.Errorf("ProcessorType = %v, want CPU (the one architecture-neutral fact known by construction)", m["ProcessorType"])
	}
}

// A host report still overrides the placeholder outright, architecture and
// all — the neutral placeholder only governs the pre-sync answer.
func TestProcessorHostReportOverridesPlaceholder(t *testing.T) {
	resetHostState(t)
	postCPU(t, `{"Socket": "CPU0", "Model": "BCM2712", "MaxSpeedMHz": 2400,
		"ProcessorArchitecture": "ARM", "InstructionSet": "ARM-A64",
		"@odata.type": "#Processor.v1_14_0.Processor"}`)

	r := hostRouter()
	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU0", lanIP, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET host-reported CPU0 = %d, body %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if m["ProcessorArchitecture"] != "ARM" {
		t.Errorf("ProcessorArchitecture = %v, want the host's reported ARM", m["ProcessorArchitecture"])
	}

	// CPU1's placeholder is gone now that the host has enumerated.
	w = do(r, http.MethodGet, "/redfish/v1/Systems/1/Processors/CPU1", lanIP, "", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET CPU1 after host enumeration = %d, want 404", w.Code)
	}
}
