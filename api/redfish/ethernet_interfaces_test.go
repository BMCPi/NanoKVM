package redfish

// Tests for the EthIp4* <-> EthernetInterface bridge: the live Bios
// attributes render onto the managed NIC member, operator PATCHes stage
// into the Bios pending settings, and host reports carrying the mapped
// properties refresh the live attributes.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// postNIC reports one NIC from the host interface, as RpiRedfishSyncDxe does.
func postNIC(t *testing.T, r *gin.Engine, body string) {
	t.Helper()
	w := do(r, http.MethodPost, "/redfish/v1/Systems/1/EthernetInterfaces",
		hostIP(t), body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST NIC = %d, body %s", w.Code, w.Body.String())
	}
}

// getNIC fetches the eth0 member from the LAN and decodes it — every caller
// in this file exercises the single onboard NIC the test fixtures set up.
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

const testNICBody = `{"Id": "eth0", "MACAddress": "2c:cf:67:00:00:01", "LinkStatus": "LinkUp"}`

func TestEthernetInterfaceOverlayStatic(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	setHostBiosAttributes(map[string]any{
		attrEthIP4Mode:       "Static",
		attrEthIP4Address:    "192.168.7.10",
		attrEthIP4SubnetMask: "255.255.255.0",
		attrEthIP4Gateway:    "192.168.7.1",
		attrEthIP4Dns1:       "192.168.7.53",
		attrEthIP4Dns2:       "",
	})

	m, code := getNIC(t, r)
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	dhcp, ok := m["DHCPv4"].(map[string]any)
	enabled, isBool := dhcp["DHCPEnabled"].(bool)
	if !ok || !isBool || enabled {
		t.Errorf("DHCPv4 = %v; want DHCPEnabled=false", m["DHCPv4"])
	}
	statics, ok := m["IPv4StaticAddresses"].([]any)
	if !ok || len(statics) != 1 {
		t.Fatalf("IPv4StaticAddresses = %v; want one entry", m["IPv4StaticAddresses"])
	}
	entry := statics[0].(map[string]any)
	if entry["Address"] != "192.168.7.10" || entry["SubnetMask"] != "255.255.255.0" ||
		entry["Gateway"] != "192.168.7.1" {
		t.Errorf("static entry = %v", entry)
	}
	dns, ok := m["StaticNameServers"].([]any)
	if !ok || len(dns) != 1 || dns[0] != "192.168.7.53" {
		t.Errorf("StaticNameServers = %v; want [192.168.7.53]", m["StaticNameServers"])
	}
	// The host's own report is passed through untouched.
	if m["LinkStatus"] != "LinkUp" {
		t.Errorf("LinkStatus = %v; report fields must survive the overlay", m["LinkStatus"])
	}
}

func TestEthernetInterfaceOverlayDhcpKeepsStatics(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	setHostBiosAttributes(map[string]any{
		attrEthIP4Mode:    "Dhcp",
		attrEthIP4Address: "192.168.7.10",
	})

	m, _ := getNIC(t, r)
	dhcp, ok := m["DHCPv4"].(map[string]any)
	enabled, isBool := dhcp["DHCPEnabled"].(bool)
	if !ok || !isBool || !enabled {
		t.Errorf("DHCPv4 = %v; want DHCPEnabled=true", m["DHCPv4"])
	}
	// Configured static address stays visible while DHCP is enabled.
	if _, ok := m["IPv4StaticAddresses"]; !ok {
		t.Error("IPv4StaticAddresses missing; configured statics should render in DHCP mode")
	}
}

func TestEthernetInterfaceOverlayUnmanaged(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	setHostBiosAttributes(map[string]any{
		attrEthIP4Mode:    "Unmanaged",
		attrEthIP4Address: "192.168.7.10",
	})

	m, _ := getNIC(t, r)
	for _, k := range []string{"DHCPv4", "IPv4StaticAddresses", "StaticNameServers"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s rendered for an Unmanaged NIC", k)
		}
	}
}

func TestEthernetInterfaceOverlaySkippedWhenAmbiguous(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	postNIC(t, r, `{"Id": "eth1", "MACAddress": "2c:cf:67:00:00:02"}`)
	setHostBiosAttributes(map[string]any{attrEthIP4Mode: "Dhcp"})

	m, _ := getNIC(t, r)
	if _, ok := m["DHCPv4"]; ok {
		t.Error("overlay applied with two members; the managed NIC is ambiguous")
	}
}

func TestPatchEthernetInterfaceStagesPending(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	// Something else already staged must survive the facade write.
	setHostBiosPending(map[string]any{"FanMode": "FixedSpeed"})

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{
			"DHCPv4": {"DHCPEnabled": false},
			"IPv4StaticAddresses": [{"Address": "10.4.0.20", "SubnetMask": "255.255.0.0", "Gateway": "10.4.0.1"}],
			"StaticNameServers": ["10.4.0.53", "10.4.0.54"]
		}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if _, ok := resp["@Message.ExtendedInfo"]; !ok {
		t.Error("response missing the apply-time message annotation")
	}

	pending := hostBiosPending()
	want := map[string]any{
		"FanMode":            "FixedSpeed",
		attrEthIP4Mode:       "Static",
		attrEthIP4Address:    "10.4.0.20",
		attrEthIP4SubnetMask: "255.255.0.0",
		attrEthIP4Gateway:    "10.4.0.1",
		attrEthIP4Dns1:       "10.4.0.53",
		attrEthIP4Dns2:       "10.4.0.54",
	}
	for k, v := range want {
		if pending[k] != v {
			t.Errorf("pending[%s] = %v; want %v", k, pending[k], v)
		}
	}
}

func TestPatchEthernetInterfaceDhcp(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"DHCPv4": {"DHCPEnabled": true}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}
	pending := hostBiosPending()
	if pending[attrEthIP4Mode] != "Dhcp" {
		t.Errorf("EthIp4Mode = %v; want Dhcp", pending[attrEthIP4Mode])
	}
	if _, ok := pending[attrEthIP4Address]; ok {
		t.Error("address staged by a DHCP-only PATCH")
	}
}

func TestPatchEthernetInterfaceStaticImpliesMode(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"IPv4StaticAddresses": [{"Address": "10.4.0.20", "SubnetMask": "255.255.0.0"}]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}
	pending := hostBiosPending()
	if pending[attrEthIP4Mode] != "Static" {
		t.Errorf("EthIp4Mode = %v; a static address should imply Static", pending[attrEthIP4Mode])
	}
	if _, ok := pending[attrEthIP4Gateway]; ok {
		t.Error("gateway staged though the entry omitted it")
	}
}

func TestPatchEthernetInterfaceClears(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"IPv4StaticAddresses": [], "StaticNameServers": []}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %s", w.Code, w.Body.String())
	}
	pending := hostBiosPending()
	for _, k := range []string{
		attrEthIP4Address, attrEthIP4SubnetMask, attrEthIP4Gateway,
		attrEthIP4Dns1, attrEthIP4Dns2,
	} {
		if v, ok := pending[k]; !ok || v != "" {
			t.Errorf("pending[%s] = %v; want \"\"", k, v)
		}
	}
	if _, ok := pending[attrEthIP4Mode]; ok {
		t.Error("clearing statics must not stage a mode change")
	}
}

func TestPatchEthernetInterfaceRejections(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)
	path := "/redfish/v1/Systems/1/EthernetInterfaces/eth0"

	for name, tc := range map[string]struct {
		path string
		body string
		want int
	}{
		"unknown member": {
			"/redfish/v1/Systems/1/EthernetInterfaces/nope",
			`{"DHCPv4": {"DHCPEnabled": true}}`, http.StatusNotFound,
		},
		"no mapped properties": {
			path, `{"HostName": "pi5"}`, http.StatusBadRequest,
		},
		"bad address": {
			path, `{"IPv4StaticAddresses": [{"Address": "not-an-ip"}]}`,
			http.StatusBadRequest,
		},
		"ipv6 address": {
			path, `{"StaticNameServers": ["2001:db8::1"]}`, http.StatusBadRequest,
		},
		"too many dns": {
			path, `{"StaticNameServers": ["10.0.0.1", "10.0.0.2", "10.0.0.3"]}`,
			http.StatusBadRequest,
		},
		"too many static addresses": {
			path, `{"IPv4StaticAddresses": [{"Address": "10.0.0.1"}, {"Address": "10.0.0.2"}]}`,
			http.StatusBadRequest,
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := do(r, http.MethodPatch, tc.path, lanIP, tc.body, nil)
			if w.Code != tc.want {
				t.Errorf("PATCH = %d, want %d; body %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	if len(hostBiosPending()) != 0 {
		t.Errorf("rejected PATCHes staged attributes: %v", hostBiosPending())
	}

	// With two members the managed NIC is ambiguous — the write is refused
	// and redirected to Bios/Settings.
	postNIC(t, r, `{"Id": "eth1", "MACAddress": "2c:cf:67:00:00:02"}`)
	w := do(r, http.MethodPatch, path, lanIP, `{"DHCPv4": {"DHCPEnabled": true}}`, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("ambiguous PATCH = %d, want 409", w.Code)
	}
}

func TestPatchEthernetInterfaceIfMatch(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	postNIC(t, r, testNICBody)

	w := do(r, http.MethodPatch, "/redfish/v1/Systems/1/EthernetInterfaces/eth0",
		lanIP, `{"DHCPv4": {"DHCPEnabled": true}}`,
		map[string]string{"If-Match": `"stale"`})
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match = %d, want 412", w.Code)
	}
	if len(hostBiosPending()) != 0 {
		t.Error("a 412 write staged attributes")
	}
}

func TestPostEthernetInterfaceRefreshesLiveAttributes(t *testing.T) {
	resetHostState(t)
	r := hostRouter()
	setHostBiosAttributes(map[string]any{"FanMode": "Automatic"})

	postNIC(t, r, `{
		"Id": "eth0",
		"MACAddress": "2c:cf:67:00:00:01",
		"DHCPv4": {"DHCPEnabled": false},
		"IPv4StaticAddresses": [{"Address": "10.4.0.20", "SubnetMask": "255.255.0.0"}]
	}`)
	attrs := hostBiosAttributes()
	if attrs[attrEthIP4Mode] != "Static" || attrs[attrEthIP4Address] != "10.4.0.20" {
		t.Errorf("live attrs = %v; want Static/10.4.0.20 merged in", attrs)
	}
	if attrs["FanMode"] != "Automatic" {
		t.Error("host NIC report clobbered unrelated live attributes")
	}
	if len(hostBiosPending()) != 0 {
		t.Error("a host report must never stage pending settings")
	}
}
