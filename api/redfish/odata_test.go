package redfish

import (
	"encoding/json"
	"testing"

	"github.com/stmcginnis/gofish/schemas"
)

// A Link must serialize to the object form. This is the whole reason the type
// exists: schemas.Link would emit a bare string here.
func TestLinkMarshalsAsODataID(t *testing.T) {
	b, err := json.Marshal(Link(systemPath))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"@odata.id":"/redfish/v1/Systems/1"}`
	if string(b) != want {
		t.Errorf("Link marshalled to %s, want %s", b, want)
	}
}

// Links must serialize as an array of objects — the Members wire form.
func TestLinksMarshalAsArrayOfODataIDs(t *testing.T) {
	b, err := json.Marshal(Links{Link("/redfish/v1/Systems/1"), Link("/redfish/v1/Systems/2")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"@odata.id":"/redfish/v1/Systems/1"},{"@odata.id":"/redfish/v1/Systems/2"}]`
	if string(b) != want {
		t.Errorf("Links marshalled to %s, want %s", b, want)
	}
}

// Our MarshalJSON output must feed back through gofish's own parser, since
// gofish clients are the reason this service exists.
func TestLinkIsReadableByGofish(t *testing.T) {
	b, err := json.Marshal(Link(biosPath))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gl schemas.Link
	if err := gl.UnmarshalJSON(b); err != nil {
		t.Fatalf("gofish unmarshal: %v", err)
	}
	if gl.String() != biosPath {
		t.Errorf("gofish read %q, want %q", gl, biosPath)
	}
}

func TestLinkRoundTrip(t *testing.T) {
	for _, form := range []struct{ name, in string }{
		{"odata.id object", `{"@odata.id":"/redfish/v1/Systems/1"}`},
		{"href object", `{"href":"/redfish/v1/Systems/1"}`},
		{"bare string", `"/redfish/v1/Systems/1"`},
	} {
		t.Run(form.name, func(t *testing.T) {
			var l Link
			if err := json.Unmarshal([]byte(form.in), &l); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if l.String() != systemPath {
				t.Errorf("got %q, want %q", l, systemPath)
			}
		})
	}
}

// Paths must derive from gofish's const, not be hand-typed.
func TestPathsDeriveFromDefaultServiceRoot(t *testing.T) {
	if ServiceRootPath != schemas.DefaultServiceRoot {
		t.Errorf("ServiceRootPath = %q, want schemas.DefaultServiceRoot (%q)",
			ServiceRootPath, schemas.DefaultServiceRoot)
	}
	for _, tc := range []struct{ got, want string }{
		{systemsPath, "/redfish/v1/Systems"},
		{systemPath, "/redfish/v1/Systems/1"},
		{biosPath, "/redfish/v1/Systems/1/Bios"},
		{managerPath, "/redfish/v1/Managers/1"},
		{sessionsPath, "/redfish/v1/SessionService/Sessions"},
		{firmwareBIOSPath, "/redfish/v1/UpdateService/FirmwareInventory/BIOS"},
		{context("Bios.Bios"), "/redfish/v1/$metadata#Bios.Bios"},
	} {
		if tc.got != tc.want {
			t.Errorf("path = %q, want %q", tc.got, tc.want)
		}
	}
}
