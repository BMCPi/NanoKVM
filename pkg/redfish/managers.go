package redfish

import (
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

func (s *Service) GetManagerCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ManagerCollection", "Manager Collection", managersPath,
		Link(managerPath),
	))
}

func (s *Service) GetManager(c *gin.Context) {
	firmwareVersion := "1.0.0"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		firmwareVersion = info.Main.Version
	}

	c.JSON(http.StatusOK, Manager{
		Resource: Resource{
			ODataType:    "#Manager.v1_11_0.Manager",
			ODataID:      managerPath,
			ODataContext: context("Manager.Manager"),
			ID:           "1",
			Name:         "NanoKVM BMC",
		},
		ManagerType:       schemas.BMCManagerType,
		FirmwareVersion:   firmwareVersion,
		Status:            &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
		SerialInterfaces:  Link(serialInterfacesPath),
		VirtualMedia:      Link(virtualMediaPath),
		NetworkInterfaces: Link(networkInterfacesPath),
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
		odataTypeKey:        "#DellAttributes.v1_0_0.DellAttributes",
		"@odata.id":         dellAttributesPath,
		"@odata.context":    context("DellAttributes.DellAttributes"),
		"Id":                "iDRAC.Embedded.1",
		"Name":              "iDRAC Attributes",
		"AttributeRegistry": "ManagerAttributeRegistry.v1_0_0",
		"Attributes": gin.H{
			"Info.1.ServerGen": "14G",
		},
	})
}
