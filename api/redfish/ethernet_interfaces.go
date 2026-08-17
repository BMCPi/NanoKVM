package redfish

// ethernet_interfaces.go serves the EthernetInterfaces collection
// (/redfish/v1/Systems/1/EthernetInterfaces). The U-Boot env that used to
// carry the host's MAC ("ethaddr") is gone with the EEPROM model, and the
// host does not report NICs over the host interface yet — so the collection
// is honestly empty rather than inventing a member. The routes stay
// registered so the ComputerSystem link resolves.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Service) GetEthernetInterfaceCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"EthernetInterfaceCollection", "Ethernet Interface Collection",
		ethernetInterfacesPath))
}

func (s *Service) GetEthernetInterface(c *gin.Context) {
	redfishErrorResponse(c, http.StatusNotFound, "no NIC inventory available")
}
