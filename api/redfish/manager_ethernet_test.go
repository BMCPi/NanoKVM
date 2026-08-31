package redfish

// manager_ethernet_test.go pins the properties an inventory crawler depends
// on. The motivating consumer is OpenCHAMI's magellan, which walks
// Managers -> EthernetInterfaces, skips any interface with no IPv4Addresses,
// and matches the remaining ones against the address it scanned to recover
// the BMC's MAC. Every assertion here protects one link in that chain.

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

func managerNICRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
		Firmware: firmware.NewController(cfg, slog.New(slog.DiscardHandler)),
	})
	r := gin.New()
	r.GET(managerEthernetInterfacesPath, svc.GetManagerEthernetInterfaceCollection)
	r.GET(managerEthernetInterfacesPath+"/:nic", svc.GetManagerEthernetInterface)
	r.GET(networkInterfacesPath, svc.GetManagerNetworkInterfaceCollection)
	r.GET(managerPath, svc.GetManager)
	return r
}

// mustGetJSON is getJSON (sensors_test.go) with the status assertion folded
// in, since every read here expects 200.
func mustGetJSON(t *testing.T, r *gin.Engine, path string) map[string]any {
	t.Helper()
	code, body := getJSON(t, r, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, code)
	}
	return body
}

// The Manager must link the collection: a crawler finds the BMC's NICs only
// by following this property.
func TestManagerLinksEthernetInterfaces(t *testing.T) {
	got := mustGetJSON(t, managerNICRouter(t), managerPath)

	link, ok := got["EthernetInterfaces"].(map[string]any)
	if !ok {
		t.Fatalf("Manager has no EthernetInterfaces link: %v", got)
	}
	if link["@odata.id"] != managerEthernetInterfacesPath {
		t.Errorf("EthernetInterfaces = %v, want %s", link["@odata.id"], managerEthernetInterfacesPath)
	}
}

// Every link the Manager advertises has to resolve. NetworkInterfaces was
// advertised for years with no route behind it; a client that follows an
// advertised link into a 404 treats the service as broken.
func TestManagerAdvertisedCollectionsResolve(t *testing.T) {
	r := managerNICRouter(t)
	mgr := mustGetJSON(t, r, managerPath)

	for _, prop := range []string{"EthernetInterfaces", "NetworkInterfaces"} {
		link, ok := mgr[prop].(map[string]any)
		if !ok {
			t.Errorf("Manager has no %s link", prop)
			continue
		}
		path, _ := link["@odata.id"].(string)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("advertised %s -> GET %s = %d, want 200", prop, path, w.Code)
		}
	}
}

// The collection must be a well-formed Redfish collection whose members all
// resolve, and each member must carry a MAC — a member without one is worse
// than no member, because a client matching on IP would adopt the blank.
func TestManagerEthernetInterfaceMembersCarryMAC(t *testing.T) {
	r := managerNICRouter(t)
	coll := mustGetJSON(t, r, managerEthernetInterfacesPath)

	members, _ := coll["Members"].([]any)
	if count, ok := coll["Members@odata.count"].(float64); !ok || int(count) != len(members) {
		t.Errorf("Members@odata.count = %v, want %d", coll["Members@odata.count"], len(members))
	}
	if len(members) == 0 {
		t.Skip("host has no non-loopback interfaces with a hardware address")
	}

	for _, m := range members {
		entry, _ := m.(map[string]any)
		path, _ := entry["@odata.id"].(string)
		if !strings.HasPrefix(path, managerEthernetInterfacesPath+"/") {
			t.Errorf("member @odata.id = %q, outside the collection", path)
			continue
		}
		nic := mustGetJSON(t, r, path)
		if mac, _ := nic["MACAddress"].(string); mac == "" {
			t.Errorf("%s has no MACAddress", path)
		}
		// IPv4Addresses must serialize as a list even when empty: a crawler
		// ranges over it directly, and null is not rangeable.
		if _, ok := nic["IPv4Addresses"].([]any); !ok {
			t.Errorf("%s IPv4Addresses = %v, want a JSON array", path, nic["IPv4Addresses"])
		}
	}
}

func TestManagerEthernetInterfaceUnknownIDIs404(t *testing.T) {
	r := managerNICRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, managerEthernetInterfacesPath+"/nope0", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown NIC = %d, want 404", w.Code)
	}
}

// Loopback must never appear: its 127.0.0.1 would match a client scanning
// over loopback and hand back lo's empty hardware address as the BMC's MAC.
func TestListManagerNICsExcludesLoopback(t *testing.T) {
	for _, nic := range listManagerNICs() {
		if ifi, err := net.InterfaceByName(nic.ID); err == nil && ifi.Flags&net.FlagLoopback != 0 {
			t.Errorf("loopback interface %q is in the manager NIC list", nic.ID)
		}
		if nic.MAC == "" {
			t.Errorf("NIC %q has an empty MAC", nic.ID)
		}
	}
}

// An IPv4 entry needs a dotted-quad SubnetMask, not a prefix length: the
// schema's type is an address string and clients render it verbatim.
func TestManagerNICIPv4SubnetMaskIsDottedQuad(t *testing.T) {
	for _, nic := range listManagerNICs() {
		for _, addr := range nic.IPv4 {
			if net.ParseIP(addr.Address) == nil {
				t.Errorf("NIC %q: Address %q is not an IP", nic.ID, addr.Address)
			}
			if addr.SubnetMask == "" {
				continue
			}
			if net.ParseIP(addr.SubnetMask) == nil {
				t.Errorf("NIC %q: SubnetMask %q is not dotted-quad", nic.ID, addr.SubnetMask)
			}
		}
	}
}
