package redfish

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
)

// Identity this BMC reports for itself. The managed host's Manufacturer and
// Model come from its own reports (hostreports.go) and are a different thing
// entirely — these describe the management controller.
const (
	managerManufacturer = "Sipeed"
	managerModel        = "NanoKVM"
)

func (s *Service) GetManagerCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ManagerCollection", "Manager Collection", managersPath,
		Link(managerPath),
	))
}

// managerFirmwareVersion is the build this BMC is running, or the "1.0.0"
// placeholder when the binary carries no stamp at all.
func managerFirmwareVersion() string {
	v := application.CurrentVersion()
	if v == "" || v == "dev" {
		return "1.0.0"
	}
	return v
}

func (s *Service) GetManager(c *gin.Context) {
	// application.CurrentVersion, not debug.ReadBuildInfo: Main.Version is
	// "(devel)" for anything built with `go build` — which is what `make app`
	// and therefore `make deploy` do — so reading it there silently fell back
	// to the "1.0.0" placeholder while the binary knew perfectly well what it
	// was. The Makefile stamps -X main.version and cmd/server assigns it to
	// application.Version; CurrentVersion reads that first and the install's
	// version file second, so it reports a real build for both a locally
	// deployed and a released image.
	//
	// The placeholder is not cosmetic: tools/conformance fails a node whose
	// FirmwareVersion is "1.0.0", and Redfish clients use it to decide whether
	// firmware is current.
	firmwareVersion := managerFirmwareVersion()

	c.JSON(http.StatusOK, Manager{
		Resource: Resource{
			ODataType:    "#Manager.v1_11_0.Manager",
			ODataID:      managerPath,
			ODataContext: odataContext("Manager.Manager"),
			ID:           "1",
			Name:         "NanoKVM BMC",
		},
		ManagerType:        schemas.BMCManagerType,
		UUID:               managerUUID(),
		Manufacturer:       managerManufacturer,
		Model:              managerModel,
		FirmwareVersion:    firmwareVersion,
		Status:             &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		SerialInterfaces:   Link(serialInterfacesPath),
		VirtualMedia:       Link(virtualMediaPath),
		EthernetInterfaces: Link(managerEthernetInterfacesPath),
		NetworkInterfaces:  Link(networkInterfacesPath),
		Links: ManagerLinks{
			ManagerForServers: Links{Link(systemPath)},
			Oem: Oem{
				"Dell": map[string]any{
					"DellAttributes": Links{Link(dellAttributesPath)},
				},
			},
		},
		Oem:     Oem{"Dell": map[string]any{}},
		Actions: Oem{"Oem": map[string]any{}},
	})
}

// GetDellIDRACAttributes serves the iDRAC.Embedded.1 attribute bag the
// Dell terraform provider needs to determine server generation. We
// claim "14G" so the provider takes the standard PATCH /Systems/1
// path (sub-17G) rather than the 17G Settings-URI flow we don't have.
func (s *Service) GetDellIDRACAttributes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		odataTypeKey:                "#DellAttributes.v1_0_0.DellAttributes",
		odataIDKey:                  dellAttributesPath,
		"@odata.context":            odataContext("DellAttributes.DellAttributes"),
		"Id":                        "iDRAC.Embedded.1",
		"Name":                      "iDRAC Attributes",
		schemaNameAttributeRegistry: "ManagerAttributeRegistry.v1_0_0",
		"Attributes": gin.H{
			"Info.1.ServerGen": "14G",
		},
	})
}
