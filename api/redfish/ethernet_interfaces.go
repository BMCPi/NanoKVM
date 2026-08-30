package redfish

// ethernet_interfaces.go serves the host's NIC inventory
// (/redfish/v1/Systems/1/EthernetInterfaces). The U-Boot env that used to
// carry the host's MAC ("ethaddr") is gone with the EEPROM model; the source
// of truth now is RpiRedfishSyncDxe, which POSTs one member per physical NIC
// each boot over the host interface, keyed on a MAC-derived Id. Until the
// first report lands the collection is honestly empty rather than inventing
// a member.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Service) GetEthernetInterfaceCollection(c *gin.Context) {
	ids := hostCollectionIDs(ethernetOf)
	links := make([]Link, 0, len(ids))
	for _, id := range ids {
		links = append(links, Link(ethernetInterfacesPath+"/"+id))
	}
	writeHostResource(c, hostView(newCollection(
		"EthernetInterfaceCollection", "Ethernet Interface Collection",
		ethernetInterfacesPath, links...)))
}

func (s *Service) GetEthernetInterface(c *gin.Context) {
	id := c.Param("nic")
	stored, ok := hostCollectionGet(ethernetOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	writeHostResource(c, renderHostMember(stored, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id))
}

// PostEthernetInterface is the host-report lane: the firmware re-POSTs its
// NIC inventory every boot. Keyed upsert — same Id, same member. POST-only by
// design: no RedfishClientPkg feature driver touches this collection and the
// hardware set is fixed, so there is no PATCH/DELETE surface to maintain.
func (s *Service) PostEthernetInterface(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	id := hostMemberID(ethernetOf, body, "NIC", "Id", "MACAddress")
	hostCollectionPut(ethernetOf, id, body)

	path := ethernetInterfacesPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeEthernetInterface,
			"EthernetInterface.EthernetInterface", id))
}
