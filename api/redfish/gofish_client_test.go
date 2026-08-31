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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// testServer mounts the read-only surface with no auth so gofish can walk it.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testDeps())
	h := testHandlers()
	r := gin.New()

	r.GET("/redfish", svc.GetRedfishBase)
	r.GET(ServiceRootPath, svc.GetServiceRoot)
	r.GET(strings.TrimSuffix(ServiceRootPath, "/"), svc.GetServiceRoot)
	r.GET(systemsPath, h.GetSystemCollection)
	r.GET(systemPath, h.GetSystem)
	r.GET(biosPath, svc.GetBios)
	r.GET(managersPath, svc.GetManagerCollection)
	r.GET(managerPath, svc.GetManager)
	r.GET(sessionServicePath, h.GetSessionService)
	r.GET(sessionsPath, h.GetSessionCollection)
	r.GET(updateServicePath, h.GetUpdateService)
	r.GET(serialInterfacesPath, h.GetSerialInterfaceCollection)
	r.GET(serialInterfacePath, h.GetSerialInterface)
	r.GET(virtualMediaPath, h.GetVirtualMediaCollection)
	r.GET(virtualMediaCDPath, h.GetVirtualMedia)

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
