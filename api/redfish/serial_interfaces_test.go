package redfish

import (
	"encoding/json"
	"testing"
)

// Redfish spells BitRate/DataBits/StopBits as string enums. We used to emit
// JSON numbers, which gofish rejects outright ("cannot unmarshal number into
// Go struct field .BitRate of type schemas.BitRate"). Pin the wire types.
func TestSerialInterfaceEmitsStringEnums(t *testing.T) {
	body := decodeBody(t, NewService().GetSerialInterface)

	for _, prop := range []string{"BitRate", "DataBits", "StopBits", "Parity", "FlowControl"} {
		v, present := body[prop]
		if !present {
			continue // omitted when unconfigured — valid
		}
		if _, ok := v.(string); !ok {
			t.Errorf("%s is %T (%v), want a string — Redfish enums are strings", prop, v, v)
		}
	}
}

// A conformant client PATCHes strings; the local UI still sends numbers. Both
// must land on the same config value.
func TestNumOrStringAcceptsBothForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"number", `{"BitRate": 115200}`, 115200},
		{"string", `{"BitRate": "115200"}`, 115200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req serialPatchRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.body, err)
			}
			if !req.BitRate.Set {
				t.Fatal("BitRate not marked set")
			}
			if req.BitRate.Value != tc.want {
				t.Errorf("BitRate = %d, want %d", req.BitRate.Value, tc.want)
			}
		})
	}
}

// An absent field must stay unset so PatchSerialInterface leaves config alone
// — the pre-migration code used 0 as the sentinel, which is why this is a
// struct and not an int.
func TestNumOrStringAbsentStaysUnset(t *testing.T) {
	var req serialPatchRequest
	if err := json.Unmarshal([]byte(`{"Parity": "Even"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.BitRate.Set || req.DataBits.Set || req.StopBits.Set {
		t.Error("absent numeric fields must not be marked set")
	}
	if req.Parity != "Even" {
		t.Errorf("Parity = %q, want Even", req.Parity)
	}
}

func TestNumOrStringRejectsGarbage(t *testing.T) {
	var req serialPatchRequest
	if err := json.Unmarshal([]byte(`{"BitRate": "fast"}`), &req); err == nil {
		t.Error("expected an error for a non-numeric string")
	}
}

// An empty string is how some clients spell "leave it alone"; it must not
// parse as 0 and zero out the baud rate.
func TestNumOrStringEmptyStringIsUnset(t *testing.T) {
	var req serialPatchRequest
	if err := json.Unmarshal([]byte(`{"BitRate": ""}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.BitRate.Set {
		t.Error(`BitRate:"" must not be treated as set`)
	}
}

// itoaOrEmpty gates the string enums: an unset config value must yield "" so
// omitempty drops the property rather than emitting an invalid empty enum.
func TestItoaOrEmpty(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{{115200, "115200"}, {8, "8"}, {0, ""}, {-1, ""}} {
		if got := itoaOrEmpty(tc.in); got != tc.want {
			t.Errorf("itoaOrEmpty(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
