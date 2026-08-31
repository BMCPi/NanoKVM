package redfish

// Tests for the vendor-agnostic EthernetInterfaces collection: the host
// feature driver creates members (POST, 201 + Location), anyone
// authenticated PATCHes standard schema properties straight onto the
// member, and the BMC stores the JSON without interpreting it — no Bios
// attributes are read or written on any lane.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// postNIC creates one member from the host interface, as the firmware's
// EthernetInterface collection driver does on its first boot.
func postNIC(t *testing.T, r *gin.Engine, body string) *string {
	t.Helper()
	w := do(r, http.MethodPost, "/redfish/v1/Systems/1/EthernetInterfaces",
		hostIP(t), body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST NIC = %d, body %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	return &loc
}

// getNIC fetches the eth0 member from the LAN and decodes it.
func getNIC(t *testing.T, r *gin.Engine) (map[string]any, int) {
	t.Helper()
	w := do(r, http.MethodGet, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, "", nil)
	m := map[string]any{}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("GET NIC: bad JSON: %v", err)
		}
	}
	return m, w.Code
}

const testNICBody = `{
	"Id": "eth0", "MACAddress": "2c:cf:67:00:00:01", "LinkStatus": "LinkUp",
	"DHCPv4": {"DHCPEnabled": true},
	"IPv4StaticAddresses": [], "StaticNameServers": []
}`

// The Location header is load-bearing: the host's feature driver records it
// into its configure-language map, and without it a BMC-side edit cannot be
// consumed until the next boot's identify pass.
func TestPostEthernetInterfaceReturnsLocation(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	loc := postNIC(t, r, testNICBody)
	if *loc != "/redfish/v1/Systems/1/EthernetInterfaces/eth0" {
		t.Errorf("Location = %q; the feature driver seeds its config-language map from this", *loc)
	}

	m, code := getNIC(t, r)
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	if m["MACAddress"] != "2c:cf:67:00:00:01" || m["LinkStatus"] != "LinkUp" {
		t.Errorf("member = %v; the POSTed report must be served back verbatim", m)
	}
	dhcp, ok := m["DHCPv4"].(map[string]any)
	if !ok || dhcp["DHCPEnabled"] != true {
		t.Errorf("DHCPv4 = %v; want the host-reported object untouched", m["DHCPv4"])
	}
}

func TestPostEthernetInterfaceUpserts(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	postNIC(t, r, `{"Id": "eth0", "MACAddress": "2c:cf:67:00:00:01", "LinkStatus": "LinkDown"}`)

	if got := len(hostCollectionIDs(ethernetOf)); got != 1 {
		t.Fatalf("members = %d; a re-report must update in place", got)
	}
	m, _ := getNIC(t, r)
	if m["LinkStatus"] != "LinkDown" {
		t.Errorf("LinkStatus = %v; want the re-report's value", m["LinkStatus"])
	}
}

// An operator PATCH merges into the stored member for the host to consume
// on its next boot. The BMC does not validate or interpret the properties —
// that is the host's job, and the whole point of the vendor-agnostic model.
func TestPatchEthernetInterfaceMergesForHostConsume(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{
			"DHCPv4": {"DHCPEnabled": false},
			"IPv4StaticAddresses": [{"Address": "10.4.0.20", "SubnetMask": "255.255.0.0", "Gateway": "10.4.0.1"}],
			"StaticNameServers": ["10.4.0.53", "10.4.0.54"]
		}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}

	m, _ := getNIC(t, r)
	dhcp, _ := m["DHCPv4"].(map[string]any)
	if dhcp["DHCPEnabled"] != false {
		t.Errorf("DHCPv4 = %v; want DHCPEnabled=false stored for the host to consume", m["DHCPv4"])
	}
	statics, ok := m["IPv4StaticAddresses"].([]any)
	if !ok || len(statics) != 1 {
		t.Fatalf("IPv4StaticAddresses = %v; want the PATCHed entry", m["IPv4StaticAddresses"])
	}
	entry := statics[0].(map[string]any)
	if entry["Address"] != "10.4.0.20" || entry["SubnetMask"] != "255.255.0.0" ||
		entry["Gateway"] != "10.4.0.1" {
		t.Errorf("static entry = %v", entry)
	}
	if dns, _ := m["IPv4StaticAddresses"]; dns == nil {
		t.Error("merge dropped the array")
	}
	// Properties the PATCH did not name survive.
	if m["MACAddress"] != "2c:cf:67:00:00:01" || m["LinkStatus"] != "LinkUp" {
		t.Errorf("unrelated properties clobbered: %v", m)
	}
	// The write never touches the Bios lanes — there is nothing to bridge.
	if len(hostBiosPending()) != 0 || len(hostBiosAttributes()) != 0 {
		t.Errorf("PATCH leaked into Bios state: pending=%v attrs=%v",
			hostBiosPending(), hostBiosAttributes())
	}
}

// The host's own Update lane is the same PATCH route (the firmware
// authenticates like any client), so a host-interface PATCH must work too.
func TestPatchEthernetInterfaceHostLane(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		hostIP(t), `{"DHCPv4": {"DHCPEnabled": true}, "IPv4StaticAddresses": []}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("host PATCH = %d, body %s", w.Code, w.Body.String())
	}
	m, _ := getNIC(t, r)
	if statics, ok := m["IPv4StaticAddresses"].([]any); !ok || len(statics) != 0 {
		t.Errorf("IPv4StaticAddresses = %v; want the cleared array stored", m["IPv4StaticAddresses"])
	}
}

func TestPatchEthernetInterfaceIdentityIsNotWritable(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"Id": "eth9", "@odata.id": "/nope"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("identity-only PATCH = %d, want 400", w.Code)
	}
	m, _ := getNIC(t, r)
	if m["Id"] != "eth0" {
		t.Errorf("Id = %v; identity keys must not be writable", m["Id"])
	}
}

func TestPatchEthernetInterfaceNotFound(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/nope",
		lanIP, `{"DHCPv4": {"DHCPEnabled": true}}`, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("PATCH unknown member = %d, want 404", w.Code)
	}
}

func TestPatchEthernetInterfaceIfMatch(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"DHCPv4": {"DHCPEnabled": false}}`,
		map[string]string{"If-Match": `"stale"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match = %d, want 412", w.Code)
	}
	m, _ := getNIC(t, r)
	if dhcp, _ := m["DHCPv4"].(map[string]any); dhcp["DHCPEnabled"] != true {
		t.Error("a 412 write changed the member")
	}
}

func TestDeleteEthernetInterface(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodDelete, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		hostIP(t), "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, body %s", w.Code, w.Body.String())
	}
	if _, code := getNIC(t, r); code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", code)
	}
}

// A host report carrying configuration properties is stored as-is and
// nothing else happens: the old EthIp4* Bios-attribute bridge is gone, and
// no lane on this collection may touch Bios state.
func TestHostReportDoesNotTouchBiosState(t *testing.T) {
	resetHostState(t)
	r := hostRouter()

	postNIC(t, r, `{
		"Id": "eth0",
		"MACAddress": "2c:cf:67:00:00:01",
		"DHCPv4": {"DHCPEnabled": false},
		"IPv4StaticAddresses": [{"Address": "10.4.0.20", "SubnetMask": "255.255.0.0"}]
	}`)
	if len(hostBiosAttributes()) != 0 {
		t.Errorf("host NIC report wrote Bios attributes: %v", hostBiosAttributes())
	}
	if len(hostBiosPending()) != 0 {
		t.Errorf("host NIC report staged Bios pending settings: %v", hostBiosPending())
	}
}
