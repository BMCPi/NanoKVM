package redfish

// gofish_client_test.go drives gofish's actual client against our handlers.
//
// This is the test the migration exists for. gofish resolves navigation
// properties through its own Link unmarshaler, so it only reaches
// /redfish/v1/Systems if our ServiceRoot emitted Systems as an
// {"@odata.id": ...} object; it only parses the ComputerSystem if our enums
// hold schema-valid values. A bare-string link or an empty enum fails here
// rather than in a customer's terraform run.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
)

// testServer mounts the read-only surface with no auth so gofish can walk it.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	r := gin.New()

	r.GET("/redfish", svc.GetRedfishBase)
	r.GET(ServiceRootPath, svc.GetServiceRoot)
	r.GET(strings.TrimSuffix(ServiceRootPath, "/"), svc.GetServiceRoot)
	r.GET(systemsPath, svc.GetSystemCollection)
	r.GET(systemPath, svc.GetSystem)
	r.GET(biosPath, svc.GetBios)
	r.GET(trustedComponentsPath, svc.GetTrustedComponentCollection)
	r.GET(bootloaderComponentPath, svc.GetTrustedComponentBootloader)
	r.GET(bootloaderSoftwarePath, svc.GetBootloaderSoftwareInventory)
	r.GET(managersPath, svc.GetManagerCollection)
	r.GET(managerPath, svc.GetManager)
	r.GET(sessionServicePath, svc.GetSessionService)
	r.GET(sessionsPath, svc.GetSessionCollection)
	r.GET(updateServicePath, svc.GetUpdateService)
	r.GET(serialInterfacesPath, svc.GetSerialInterfaceCollection)
	r.GET(serialInterfacePath, svc.GetSerialInterface)
	r.GET(virtualMediaPath, svc.GetVirtualMediaCollection)
	r.GET(virtualMediaCDPath, svc.GetVirtualMedia)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// ConnectDefault GETs schemas.DefaultServiceRoot — proving the trailing-slash
// route is the one a real client asks for.
func TestGofishConnectAndDiscover(t *testing.T) {
	ts := testServer(t)

	client, err := gofish.ConnectDefault(ts.URL)
	if err != nil {
		t.Fatalf("gofish.ConnectDefault: %v", err)
	}
	defer client.Logout()

	root := client.GetService()
	if root == nil {
		t.Fatal("gofish parsed no service root")
	}

	// Following Systems requires our ServiceRoot to have emitted the link as
	// an @odata.id object; gofish reaches nothing otherwise.
	systems, err := root.Systems()
	if err != nil {
		t.Fatalf("gofish could not follow the Systems link: %v", err)
	}
	if len(systems) != 1 {
		t.Fatalf("discovered %d systems, want 1", len(systems))
	}

	sys := systems[0]
	if sys.ID != "1" {
		t.Errorf("system Id = %q, want 1", sys.ID)
	}
	if sys.SystemType != schemas.PhysicalSystemType {
		t.Errorf("SystemType = %q, want Physical", sys.SystemType)
	}
	// PowerState must be a valid member — "" would mean we emitted an empty
	// enum, which is exactly what marshalling a gofish struct produces.
	if sys.PowerState != schemas.OnPowerState && sys.PowerState != schemas.OffPowerState {
		t.Errorf("PowerState = %q, want On or Off", sys.PowerState)
	}

	managers, err := root.Managers()
	if err != nil {
		t.Fatalf("gofish could not follow the Managers link: %v", err)
	}
	if len(managers) != 1 {
		t.Fatalf("discovered %d managers, want 1", len(managers))
	}
	if managers[0].ManagerType != schemas.BMCManagerType {
		t.Errorf("ManagerType = %q, want BMC", managers[0].ManagerType)
	}
}

// The Bios link is the property gofish's ComputerSystem cannot even express
// (the field is unexported), so verify the client resolves it off the wire.
func TestGofishFollowsBiosLink(t *testing.T) {
	ts := testServer(t)

	client, err := gofish.ConnectDefault(ts.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Logout()

	systems, err := client.GetService().Systems()
	if err != nil || len(systems) == 0 {
		t.Fatalf("systems: %v", err)
	}

	// gofish populates ComputerSystem.bios from our @odata.id; Bios() then
	// GETs it. A bare-string link would leave the field empty and error.
	if _, err := systems[0].Bios(); err != nil {
		t.Errorf("gofish could not follow the Bios link: %v", err)
	}
}

// The bootloader is exposed as a TrustedComponent (root of trust) with its
// firmware as a nested SoftwareInventory. gofish must follow
// System → Links.TrustedComponents → ActiveSoftwareImage end to end.
func TestGofishTrustedComponentsAndSoftwareImage(t *testing.T) {
	ts := testServer(t)

	client, err := gofish.ConnectDefault(ts.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Logout()

	systems, err := client.GetService().Systems()
	if err != nil || len(systems) == 0 {
		t.Fatalf("systems: %v", err)
	}

	comps, err := systems[0].TrustedComponents()
	if err != nil {
		t.Fatalf("gofish could not follow Links.TrustedComponents: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("discovered %d trusted components, want 1", len(comps))
	}
	if comps[0].TrustedComponentType != schemas.IntegratedTrustedComponentType {
		t.Errorf("TrustedComponentType = %q, want Integrated", comps[0].TrustedComponentType)
	}
	if comps[0].Manufacturer != "Raspberry Pi" {
		t.Errorf("Manufacturer = %q, want Raspberry Pi", comps[0].Manufacturer)
	}

	// The nested SoftwareInventory carries the bootloader version/flash-time.
	img, err := comps[0].ActiveSoftwareImage()
	if err != nil {
		t.Fatalf("gofish could not follow ActiveSoftwareImage: %v", err)
	}
	if img.SoftwareID != "rpi-eeprom" {
		t.Errorf("SoftwareId = %q, want rpi-eeprom", img.SoftwareID)
	}
	if !img.Updateable {
		t.Error("SoftwareInventory should be Updateable (BMC stages pieeprom.upd)")
	}
}

// The MemorySummary we synthesise from the SMBIOS type 16/17 tables must be
// readable by gofish's own ComputerSystem schema — the same contract the rest
// of this file pins for the live surface. The store is unconfigured under test,
// so exercise the mapping directly and round-trip the marshalled system.
func TestGofishParsesMemorySummary(t *testing.T) {
	sys := ComputerSystem{
		Resource: Resource{
			ODataType: "#ComputerSystem.v1_13_0.ComputerSystem",
			ODataID:   systemPath,
			ID:        "1",
			Name:      "Computer System",
		},
	}
	applySMBIOSInfo(&sys, &smbios.Info{
		MemoryTotalMB: 16384,
		Memory: []smbios.MemoryModule{{
			Locator: "P0", SizeMB: 16384, Type: "LPDDR4", SpeedMTs: 4267,
		}},
	})

	raw, err := json.Marshal(sys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var gsys schemas.ComputerSystem
	if err := json.Unmarshal(raw, &gsys); err != nil {
		t.Fatalf("gofish cannot parse our ComputerSystem: %v\nbody: %s", err, raw)
	}
	if gsys.MemorySummary.TotalSystemMemoryGiB == nil {
		t.Fatal("gofish read no TotalSystemMemoryGiB")
	}
	if got := *gsys.MemorySummary.TotalSystemMemoryGiB; got != 16 {
		t.Errorf("TotalSystemMemoryGiB round-trip = %v, want 16", got)
	}
	if gsys.MemorySummary.MemoryMirroring != schemas.NoneMemoryMirroring {
		t.Errorf("MemoryMirroring = %q, want None", gsys.MemorySummary.MemoryMirroring)
	}
}

// SerialInterface's BitRate/DataBits/StopBits are string enums. Before the
// migration we emitted JSON numbers, which fails gofish's unmarshal outright.
func TestGofishParsesSerialInterface(t *testing.T) {
	ts := testServer(t)

	client, err := gofish.ConnectDefault(ts.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Logout()

	managers, err := client.GetService().Managers()
	if err != nil || len(managers) == 0 {
		t.Fatalf("managers: %v", err)
	}

	// This is the call that used to fail with:
	//   cannot unmarshal number into Go struct field .BitRate of type schemas.BitRate
	ifaces, err := managers[0].SerialInterfaces()
	if err != nil {
		t.Fatalf("gofish could not parse our SerialInterface: %v", err)
	}
	if len(ifaces) != 1 {
		t.Fatalf("discovered %d serial interfaces, want 1", len(ifaces))
	}
}

// VirtualMedia carries the ConnectedVia enum the Dell terraform provider reads.
func TestGofishParsesVirtualMedia(t *testing.T) {
	ts := testServer(t)

	client, err := gofish.ConnectDefault(ts.URL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Logout()

	managers, err := client.GetService().Managers()
	if err != nil || len(managers) == 0 {
		t.Fatalf("managers: %v", err)
	}

	media, err := managers[0].VirtualMedia()
	if err != nil {
		t.Fatalf("gofish could not parse our VirtualMedia: %v", err)
	}
	if len(media) != 1 {
		t.Fatalf("discovered %d virtual media, want 1", len(media))
	}
	if media[0].ConnectedVia != schemas.NotConnectedConnectedVia &&
		media[0].ConnectedVia != schemas.URIConnectedVia {
		t.Errorf("ConnectedVia = %q, want NotConnected or URI", media[0].ConnectedVia)
	}
}
