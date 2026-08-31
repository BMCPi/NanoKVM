package redfish

// ethernet_interfaces.go serves the host's NIC inventory
// (/redfish/v1/Systems/1/EthernetInterfaces). The U-Boot env that used to
// carry the host's MAC ("ethaddr") is gone with the EEPROM model; the source
// of truth now is RpiRedfishSyncDxe, which POSTs one member per physical NIC
// each boot over the host interface, keyed on the Linux-style ordinal Id it
// assigns ("eth0"; the MAC travels in the MACAddress property). Until the
// first report lands the collection is honestly empty rather than inventing
// a member.
//
// On top of the inventory, this file bridges the host's EthConfigDxe Bios
// attributes into EthernetInterface terms. The host firmware exposes the
// onboard NIC's IPv4 policy as Bios attributes (x-UEFI-redfish-Bios.v1_1_0,
// applied to the NIC's Ip4Config2 on every boot):
//
//	EthIp4Mode        "Unmanaged" | "Dhcp" | "Static"
//	EthIp4Address     dotted quad, "" = unset
//	EthIp4SubnetMask  dotted quad
//	EthIp4Gateway     dotted quad, optional
//	EthIp4Dns1/Dns2   dotted quad, optional
//
// mapped to and from the standard EthernetInterface properties:
//
//	EthIp4Mode                       <->  DHCPv4.DHCPEnabled
//	EthIp4Address/SubnetMask/Gateway <->  IPv4StaticAddresses[0]
//	EthIp4Dns1, EthIp4Dns2           <->  StaticNameServers
//
// GET renders the live attributes onto the managed member; PATCH (operator)
// stages writes into the Bios pending settings for the host's next boot; a
// host POST that carries the mapped properties refreshes the live attributes.

import (
	"fmt"
	"net"
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
	member := renderHostMember(stored, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id)
	if managed, ok := ethManagedNICID(); ok && managed == id {
		ethOverlayBiosConfig(member)
	}
	writeHostResource(c, member)
}

// PostEthernetInterface is the host-report lane: the firmware re-POSTs its
// NIC inventory every boot. Keyed upsert — same Id, same member. A report
// that carries the mapped IPv4 configuration properties also refreshes the
// live EthIp4* Bios attributes (merge — the host lane reports fact, same as
// its Bios PATCH); a value the bridge cannot map never rejects the inventory
// report itself.
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
	hostCollectionPut(ethernetOf, id, body)
	if attrs, err := ethBiosAttrsFromBody(body); err == nil && len(attrs) > 0 {
		mergeHostBiosAttributes(attrs)
	}

	path := ethernetInterfacesPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeEthernetInterface,
			"EthernetInterface.EthernetInterface", id))
}

// PatchEthernetInterface is the operator's configuration surface: the mapped
// EthernetInterface properties are translated onto the EthIp4* Bios
// attributes and staged into the Bios pending settings, which the host
// firmware consumes and applies on its next boot. Normal authentication, not
// hostWritable — like PATCH /Bios/Settings, this is an operator instruction,
// and staging (merge) must not clobber unrelated attributes already staged
// there. The resource itself does not change until the host applies and
// re-reports, so the response echoes the current view plus an apply-time
// message.
func (s *Service) PatchEthernetInterface(c *gin.Context) {
	id := c.Param("nic")
	stored, ok := hostCollectionGet(ethernetOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	if managed, ok := ethManagedNICID(); !ok || managed != id {
		redfishErrorResponse(c, http.StatusConflict,
			"cannot tell which NIC the host firmware manages; "+
				"PATCH /redfish/v1/Systems/1/Bios/Settings with EthIp4* attributes instead")
		return
	}
	current := renderHostMember(stored, ethernetInterfacesPath+"/"+id, id,
		odataTypeEthernetInterface, "EthernetInterface.EthernetInterface", id)
	ethOverlayBiosConfig(current)
	if !hostCheckIfMatch(c, current) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	attrs, err := ethBiosAttrsFromBody(body)
	if err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(attrs) == 0 {
		redfishErrorResponse(c, http.StatusBadRequest,
			"no writable properties in request; supported: "+
				"DHCPv4.DHCPEnabled, IPv4StaticAddresses, StaticNameServers")
		return
	}
	mergeHostBiosPending(attrs)

	current["@Message.ExtendedInfo"] = []MessageInfo{{
		MessageID: "Base.1.13.SettingsApplyTime",
		Message:   "IPv4 settings staged as Bios attributes; the host firmware applies them on its next boot.",
		Severity:  "OK",
	}}
	writeHostResource(c, current)
}

// --- EthIp4* Bios attribute bridge -------------------------------------------

const (
	attrEthIp4Mode       = "EthIp4Mode"
	attrEthIp4Address    = "EthIp4Address"
	attrEthIp4SubnetMask = "EthIp4SubnetMask"
	attrEthIp4Gateway    = "EthIp4Gateway"
	attrEthIp4Dns1       = "EthIp4Dns1"
	attrEthIp4Dns2       = "EthIp4Dns2"
)

// ethManagedNICID returns the member the host's EthConfigDxe manages. The
// firmware claims "the first NIC that is not the BMC's USB gadget" — an
// ordering the BMC cannot observe — but the reported collection excludes USB
// NICs entirely and this board has one onboard NIC, so the bridge is offered
// exactly while the collection holds a single member. With zero or several
// members the EthIp4* attributes remain reachable through the Bios resource.
func ethManagedNICID() (string, bool) {
	ids := hostCollectionIDs(ethernetOf)
	if len(ids) != 1 {
		return "", false
	}
	return ids[0], true
}

// ethOverlayBiosConfig renders the live EthIp4* Bios attributes onto a
// rendered member. Unmanaged (or unreported) attributes render nothing: the
// firmware is not managing the NIC, so claiming a DHCP/static policy here
// would be an invention. The stored static address and name servers are
// rendered in both modes — IPv4StaticAddresses is configuration, not state,
// and it stays configured while DHCP is enabled.
func ethOverlayBiosConfig(member map[string]any) {
	attrs := hostBiosAttributes()
	switch attrs[attrEthIp4Mode] {
	case "Dhcp":
		member["DHCPv4"] = map[string]any{"DHCPEnabled": true}
	case "Static":
		member["DHCPv4"] = map[string]any{"DHCPEnabled": false}
	default:
		return
	}

	static := map[string]any{}
	for prop, attr := range map[string]string{
		"Address":    attrEthIp4Address,
		"SubnetMask": attrEthIp4SubnetMask,
		"Gateway":    attrEthIp4Gateway,
	} {
		if v, _ := attrs[attr].(string); v != "" {
			static[prop] = v
		}
	}
	if len(static) > 0 {
		member["IPv4StaticAddresses"] = []any{static}
	}

	dns := []any{}
	for _, attr := range []string{attrEthIp4Dns1, attrEthIp4Dns2} {
		if v, _ := attrs[attr].(string); v != "" {
			dns = append(dns, v)
		}
	}
	if len(dns) > 0 {
		member["StaticNameServers"] = dns
	}
}

// ethBiosAttrsFromBody extracts the EthIp4* attribute updates an
// EthernetInterface write carries. An empty map means the body had none of
// the mapped properties. Values are validated as dotted-quad IPv4 ("" and
// JSON null clear a field) so a typo is a 400 at the API rather than a
// silently unapplied variable on the host.
func ethBiosAttrsFromBody(body map[string]any) (map[string]any, error) {
	attrs := map[string]any{}

	if raw, ok := body["DHCPv4"]; ok {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("DHCPv4 must be an object")
		}
		if en, ok := obj["DHCPEnabled"]; ok {
			b, ok := en.(bool)
			if !ok {
				return nil, fmt.Errorf("DHCPv4.DHCPEnabled must be a boolean")
			}
			if b {
				attrs[attrEthIp4Mode] = "Dhcp"
			} else {
				attrs[attrEthIp4Mode] = "Static"
			}
		}
	}

	if raw, ok := body["IPv4StaticAddresses"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("IPv4StaticAddresses must be an array")
		}
		// Null elements mean "leave this position unchanged" (DSP0266);
		// extra real entries are refused rather than dropped, because the
		// firmware variable holds exactly one static address.
		var entry map[string]any
		entries := 0
		for _, el := range list {
			if el == nil {
				continue
			}
			obj, ok := el.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("IPv4StaticAddresses entries must be objects or null")
			}
			entry = obj
			entries++
		}
		switch {
		case entries > 1:
			return nil, fmt.Errorf("only one IPv4 static address is supported")
		case entries == 0 && len(list) == 0:
			// An empty array clears the static address.
			attrs[attrEthIp4Address] = ""
			attrs[attrEthIp4SubnetMask] = ""
			attrs[attrEthIp4Gateway] = ""
		case entries == 1:
			for prop, attr := range map[string]string{
				"Address":    attrEthIp4Address,
				"SubnetMask": attrEthIp4SubnetMask,
				"Gateway":    attrEthIp4Gateway,
			} {
				v, present := entry[prop]
				if !present {
					continue
				}
				s, err := ethIPString(prop, v)
				if err != nil {
					return nil, err
				}
				attrs[attr] = s
			}
			// A static address without an explicit DHCPv4 object implies
			// static mode; an explicit DHCPEnabled wins either way.
			if _, ok := attrs[attrEthIp4Mode]; !ok {
				attrs[attrEthIp4Mode] = "Static"
			}
		}
	}

	if raw, ok := body["StaticNameServers"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("StaticNameServers must be an array")
		}
		if len(list) > 2 {
			return nil, fmt.Errorf("at most two static name servers are supported")
		}
		dns := []string{"", ""}
		for i, el := range list {
			s, err := ethIPString("StaticNameServers", el)
			if err != nil {
				return nil, err
			}
			dns[i] = s
		}
		attrs[attrEthIp4Dns1] = dns[0]
		attrs[attrEthIp4Dns2] = dns[1]
	}

	return attrs, nil
}

// ethIPString validates one write value: JSON null or "" clears, anything
// else must be a dotted-quad IPv4 address.
func ethIPString(prop string, v any) (string, error) {
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s values must be strings", prop)
	}
	if s == "" {
		return "", nil
	}
	if ip := net.ParseIP(s); ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("%s: %q is not an IPv4 dotted-quad address", prop, s)
	}
	return s, nil
}
