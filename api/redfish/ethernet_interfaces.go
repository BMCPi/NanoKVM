package redfish

// ethernet_interfaces.go serves /redfish/v1/Systems/1/EthernetInterfaces as
// a plain Redfish resource collection with no vendor-specific knowledge.
//
// The host firmware's EthernetInterface feature driver
// (rpi5-uefi-build: RedfishEthernetInterfaceDxe + its collection driver)
// owns the members: it POSTs the member it manages on first boot (the
// Location header seeds its configure-language map, so it must be present),
// GETs the member each boot to consume configuration changes, and PATCHes
// its current values back. An operator configures the NIC by PATCHing the
// standard schema properties (DHCPv4.DHCPEnabled, IPv4StaticAddresses,
// StaticNameServers) straight onto the member; the BMC merges the write
// into the stored JSON without interpreting it, and the host consumes and
// applies it on its next boot (direct-resource model — no
// @Redfish.Settings staging, and validation belongs to the host, which is
// the party that knows what its network stack accepts).
//
// The EthIp4* Bios-attribute bridge that used to live here is gone with
// the firmware's move off Bios attributes for NIC config: the BMC no
// longer maps, validates, or even recognizes any of these properties.

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

// operatorNICProperties are the member properties an operator writes and the
// host consumes, rather than ones the host reports. A re-POST carries them
// forward when the report omits them.
//
// The firmware POSTs its whole NIC report on boot, and that report describes
// what the adapter IS — MAC, link state, the addresses currently in effect —
// not what it has been asked to become. A plain replace therefore erased any
// configuration staged since the last boot, in the very window before the
// host had read it. Processors has the same operator/host shared-member shape
// and the same fix (see hostCollectionPutPreserving).
//
// Once the host has applied a change it reports the new value back, and that
// report wins: preservation only covers keys the host said nothing about.
var operatorNICProperties = []string{
	"DHCPv4",
	"IPv4StaticAddresses",
	"StaticNameServers",
	"MTUSize",
}

// PostEthernetInterface is the host's member-creation lane. Keyed upsert —
// same Id, same member. The 201 + Location contract is load-bearing: the
// feature driver records the Location URI into its configure-language map,
// and without it a BMC-side edit cannot be consumed until the next boot's
// identify pass.
func (s *Service) PostEthernetInterface(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	// Preference order tracks the firmware contract: the eth<N> Id it
	// assigns, then the MAC (older reports), then a generated ethN.
	id := hostMemberID(ethernetOf, body, "eth", "Id", "MACAddress")
	hostCollectionPutPreserving(ethernetOf, id, body, operatorNICProperties...)

	path := ethernetInterfacesPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeEthernetInterface,
			"EthernetInterface.EthernetInterface", id))
}

// PatchEthernetInterface merges a write into the stored member — the host
// updating its report, or an operator staging configuration the host will
// consume on its next boot. Top-level properties replace whole (object and
// array values included), the DSP0266 default for a service that does not
// model the schema; identity keys are not writable.
func (s *Service) PatchEthernetInterface(c *gin.Context) {
	id := c.Param("nic")
	stored, ok := hostCollectionGet(ethernetOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(stored, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id)) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	for _, k := range []string{"Id", odataIDKey, odataTypeKey, "@odata.context"} {
		delete(body, k)
	}
	if len(body) == 0 {
		redfishErrorResponse(c, http.StatusBadRequest, "no writable properties in request")
		return
	}
	merged := hostCollectionMerge(ethernetOf, id, body)
	if merged == nil {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	writeHostResource(c, renderHostMember(merged, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id))
}

// DeleteEthernetInterface retires one host-reported NIC member. Host-lane
// only, same stale-member contract as DeleteBootOption: the collection
// persists, so only the host can retire a member it no longer manages.
func (s *Service) DeleteEthernetInterface(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("nic")
	current, ok := hostCollectionGet(ethernetOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id)) {
		return
	}
	hostCollectionDelete(ethernetOf, id)
	c.Status(http.StatusNoContent)
}
