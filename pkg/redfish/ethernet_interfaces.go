package redfish

// ethernet_interfaces.go exposes the host's NIC as the standard
// EthernetInterfaces collection (/redfish/v1/Systems/1/EthernetInterfaces).
// The EthernetInterface resource is the DSP2046 home for the MAC address the
// U-Boot env carries as "ethaddr" — previously reported under Oem.NanoKVM
// because ComputerSystem itself has no MACAddress property.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// ethernetInterfaceID names the single onboard NIC the env describes.
const ethernetInterfaceID = "eth0"

// EthernetInterface is the Redfish EthernetInterface resource (DSP2046 §6.24).
type EthernetInterface struct {
	Resource
	MACAddress          string  `json:"MACAddress,omitempty"`
	PermanentMACAddress string  `json:"PermanentMACAddress,omitempty"`
	Status              *Status `json:"Status,omitempty"`
}

// hostMACAddress returns the managed host's onboard MAC, or "".
func hostMACAddress() string {
	inv, err := firmware.GetController().GetInventory()
	if err != nil {
		return ""
	}
	return inv["ethaddr"]
}

func (s *Service) GetEthernetInterfaceCollection(c *gin.Context) {
	var links []Link
	if hostMACAddress() != "" {
		links = append(links, Link(ethernetInterfacePath))
	}
	c.JSON(http.StatusOK, newCollection(
		"EthernetInterfaceCollection", "Ethernet Interface Collection",
		ethernetInterfacesPath, links...))
}

func (s *Service) GetEthernetInterface(c *gin.Context) {
	if c.Param("nic") != ethernetInterfaceID {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found")
		return
	}
	mac := hostMACAddress()
	if mac == "" {
		redfishErrorResponse(c, http.StatusNotFound, "no NIC inventory available")
		return
	}
	c.JSON(http.StatusOK, EthernetInterface{
		Resource: Resource{
			ODataType:    "#EthernetInterface.v1_9_0.EthernetInterface",
			ODataID:      ethernetInterfacePath,
			ODataContext: context("EthernetInterface.EthernetInterface"),
			ID:           ethernetInterfaceID,
			Name:         "Onboard Ethernet",
		},
		MACAddress: mac,
		// ethaddr is the factory-programmed address, so it is also the
		// permanent one.
		PermanentMACAddress: mac,
		Status:              &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	})
}
