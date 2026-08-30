package redfish

// manager_ethernet.go serves the BMC's own network inventory:
// /redfish/v1/Managers/1/EthernetInterfaces.
//
// This is the one collection on this service the BMC is the sole authority
// for — unlike Systems/1/EthernetInterfaces, which waits on the managed host
// to report its NICs, the manager's interfaces are this device's own and are
// read straight from the kernel.
//
// Inventory tools depend on it more than the name suggests. OpenCHAMI's
// magellan always sends SMD `"MACRequired": true`, and fills `MACAddr` only
// by walking Managers -> EthernetInterfaces and matching an IPv4Addresses
// entry against the address it scanned; an interface carrying no IPv4Addresses
// is skipped outright. So the MAC alone is not enough — the address the client
// reached us on has to appear here too, or the endpoint is registered without
// the MAC it declared mandatory.

import (
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// EthernetInterface is the Redfish EthernetInterface resource (DSP2046 §6.28)
// as served for the BMC's own NICs.
type EthernetInterface struct {
	Resource
	MACAddress          string            `json:"MACAddress,omitempty"`
	PermanentMACAddress string            `json:"PermanentMACAddress,omitempty"`
	InterfaceEnabled    bool              `json:"InterfaceEnabled"`
	LinkStatus          string            `json:"LinkStatus,omitempty"`
	MTUSize             int               `json:"MTUSize,omitempty"`
	HostName            string            `json:"HostName,omitempty"`
	IPv4Addresses       []IPv4Address     `json:"IPv4Addresses"`
	IPv6Addresses       []IPv6Address     `json:"IPv6Addresses,omitempty"`
	Status              *Status           `json:"Status,omitempty"`
	Links               *EthernetIfcLinks `json:"Links,omitempty"`
}

// IPv4Address is one address on an interface. SubnetMask is dotted-quad per
// the schema, not a prefix length.
type IPv4Address struct {
	Address       string `json:"Address"`
	SubnetMask    string `json:"SubnetMask,omitempty"`
	AddressOrigin string `json:"AddressOrigin,omitempty"`
	Gateway       string `json:"Gateway,omitempty"`
}

type IPv6Address struct {
	Address       string `json:"Address"`
	PrefixLength  int    `json:"PrefixLength,omitempty"`
	AddressOrigin string `json:"AddressOrigin,omitempty"`
}

// EthernetIfcLinks binds the interface back to the manager that owns it.
type EthernetIfcLinks struct {
	OwningManager *Link `json:"OwningManager,omitempty"`
}

// managerNIC is one enumerated kernel interface, before it is rendered as a
// Redfish resource.
type managerNIC struct {
	ID   string
	Name string
	MAC  string
	MTU  int
	Up   bool
	IPv4 []IPv4Address
	IPv6 []IPv6Address
}

// isPhysicalNIC reports whether name is backed by a hardware device.
//
// The kernel exposes /sys/class/net/<name>/device only for interfaces with a
// real parent device, which is the standard way to tell a NIC from a bridge,
// veth, bond, or vlan. On the BMC itself the distinction barely matters — the
// device has eth0 and the gadget link and nothing else — but the same binary
// runs on developer workstations, where an unfiltered enumeration reports
// docker0 and a dozen veth pairs as BMC inventory.
func isPhysicalNIC(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/device")
	return err == nil
}

// listManagerNICs enumerates the BMC's usable network interfaces.
//
// Loopback is excluded (it is not inventory, and its 127.0.0.1 would match a
// loopback-scanning client and hand back an empty MAC). Interfaces with no
// hardware address are excluded for the same reason: an entry whose MAC is
// blank is worse than no entry, because a client matching on IP would adopt
// the blank.
//
// Virtual interfaces are then filtered out — but only if doing so leaves
// something behind. An empty collection is the one outcome that actively
// breaks a discovery client, so a kernel that does not expose the sysfs
// device links falls back to reporting everything rather than nothing.
func listManagerNICs() []managerNIC {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	nics := make([]managerNIC, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		mac := ifi.HardwareAddr.String()
		if mac == "" {
			continue
		}

		nic := managerNIC{
			ID:   ifi.Name,
			Name: ifi.Name,
			MAC:  mac,
			MTU:  ifi.MTU,
			Up:   ifi.Flags&net.FlagUp != 0,
		}

		addrs, err := ifi.Addrs()
		if err != nil {
			// Report the interface anyway: the MAC is still inventory, and a
			// transient netlink error should not make a NIC disappear.
			addrs = nil
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				nic.IPv4 = append(nic.IPv4, IPv4Address{
					Address:    v4.String(),
					SubnetMask: net.IP(ipnet.Mask).String(),
					// The BMC does not track how each address was acquired,
					// and the schema has no "unknown" member. DHCP is the
					// honest default for this device: eth0 is a DHCP client
					// out of the box (see pkg/network).
					AddressOrigin: "DHCP",
				})
				continue
			}
			if ipnet.IP.IsLinkLocalUnicast() {
				continue // fe80:: tells an inventory nothing
			}
			ones, _ := ipnet.Mask.Size()
			nic.IPv6 = append(nic.IPv6, IPv6Address{
				Address:       ipnet.IP.String(),
				PrefixLength:  ones,
				AddressOrigin: "SLAAC",
			})
		}
		nics = append(nics, nic)
	}

	if physical := filterPhysical(nics); len(physical) > 0 {
		nics = physical
	}

	// Stable order so the collection does not reshuffle between GETs.
	sort.Slice(nics, func(i, j int) bool { return nics[i].ID < nics[j].ID })
	return nics
}

func filterPhysical(nics []managerNIC) []managerNIC {
	out := make([]managerNIC, 0, len(nics))
	for _, nic := range nics {
		if isPhysicalNIC(nic.ID) {
			out = append(out, nic)
		}
	}
	return out
}

// findManagerNIC returns the NIC whose Redfish ID matches id.
func findManagerNIC(id string) (managerNIC, bool) {
	for _, nic := range listManagerNICs() {
		if nic.ID == id {
			return nic, true
		}
	}
	return managerNIC{}, false
}

func (s *Service) GetManagerEthernetInterfaceCollection(c *gin.Context) {
	nics := listManagerNICs()
	members := make(Links, 0, len(nics))
	for _, nic := range nics {
		members = append(members, Link(managerEthernetInterfacesPath+"/"+nic.ID))
	}
	c.JSON(http.StatusOK, newCollection(
		"EthernetInterfaceCollection", "Manager Ethernet Interface Collection",
		managerEthernetInterfacesPath, members...))
}

func (s *Service) GetManagerEthernetInterface(c *gin.Context) {
	id := c.Param("nic")
	nic, ok := findManagerNIC(id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "ethernet interface not found: "+id)
		return
	}
	c.JSON(http.StatusOK, buildManagerEthernetInterface(nic))
}

func buildManagerEthernetInterface(nic managerNIC) EthernetInterface {
	linkStatus := "LinkDown"
	state := schemas.DisabledState
	if nic.Up {
		linkStatus = "LinkUp"
		state = schemas.EnabledState
	}

	owner := Link(managerPath)
	ipv4 := nic.IPv4
	if ipv4 == nil {
		// Must serialize as [] rather than null: clients iterate it directly.
		ipv4 = []IPv4Address{}
	}

	return EthernetInterface{
		Resource: Resource{
			ODataType:    "#EthernetInterface.v1_6_0.EthernetInterface",
			ODataID:      managerEthernetInterfacesPath + "/" + nic.ID,
			ODataContext: odataContext("EthernetInterface.EthernetInterface"),
			ID:           nic.ID,
			Name:         "Manager Ethernet Interface",
			Description:  "BMC network interface " + nic.Name,
		},
		MACAddress:          nic.MAC,
		PermanentMACAddress: nic.MAC,
		InterfaceEnabled:    nic.Up,
		LinkStatus:          linkStatus,
		MTUSize:             nic.MTU,
		HostName:            managerHostName(),
		IPv4Addresses:       ipv4,
		IPv6Addresses:       nic.IPv6,
		Status:              &Status{State: state, Health: schemas.OKHealth},
		Links:               &EthernetIfcLinks{OwningManager: &owner},
	}
}

// managerHostName is the BMC's hostname, best-effort.
func managerHostName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// GetManagerNetworkInterfaceCollection serves the NetworkInterfaces link the
// Manager has always advertised. The collection is empty and always will be:
// NetworkInterface describes NetworkAdapter hardware (a smart NIC with its own
// controller), which this BMC does not have. It exists so the advertised link
// resolves — a client that follows a link into a 404 concludes the service is
// broken, whereas an empty collection is a complete answer.
func (s *Service) GetManagerNetworkInterfaceCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"NetworkInterfaceCollection", "Manager Network Interface Collection",
		networkInterfacesPath))
}
