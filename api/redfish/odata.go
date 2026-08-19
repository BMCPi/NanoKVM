package redfish

// odata.go carries the OData plumbing shared by every Redfish resource:
// the navigation-link type and the canonical resource paths.
//
// We take our vocabulary from github.com/stmcginnis/gofish/schemas — the
// enums (schemas.BootSource, schemas.ResetType, ...) and the service root
// const — but not its structs. gofish is a *client* library: it parses
// Redfish payloads and never emits them, so none of its 200-odd schema
// types define MarshalJSON, their navigation links are unexported (
// ComputerSystem.bios and friends can't be set from outside the package),
// and their fields mostly lack omitempty — marshalling one back out
// produces empty strings where the schema requires a valid enum. Serving
// those bytes would be worse than the gin.H maps they replaced.
//
// So the types here own the wire format and borrow gofish's vocabulary.

import (
	"encoding/json"

	"github.com/stmcginnis/gofish/schemas"
)

// Link is an OData navigation reference.
//
// It mirrors schemas.Link — same underlying string, same "accept
// @odata.id or href" parse — but supplies the MarshalJSON that gofish
// omits. Marshalling a schemas.Link yields the bare string
// "/redfish/v1/Systems/1"; a service has to emit the object form
// {"@odata.id": "/redfish/v1/Systems/1"}, which is what this produces.
type Link string

// odataRef is the wire form of a navigation property.
type odataRef struct {
	ODataID string `json:"@odata.id"`
}

// MarshalJSON renders the link as {"@odata.id": "..."}.
func (l Link) MarshalJSON() ([]byte, error) {
	return json.Marshal(odataRef{ODataID: string(l)})
}

// UnmarshalJSON accepts the {"@odata.id": ...} / {"href": ...} object form
// via gofish, and additionally the bare-string form. Keeping both means a
// Link survives a round-trip through MarshalJSON above, which is what the
// handler tests assert.
func (l *Link) UnmarshalJSON(b []byte) error {
	// schemas.Link.UnmarshalJSON never reports an error: on a non-object
	// it just leaves the link empty. So switch on the result, not the err.
	var gl schemas.Link
	_ = gl.UnmarshalJSON(b)
	if gl != "" {
		*l = Link(gl)
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = Link(s)
		return nil
	}

	*l = ""
	return nil
}

func (l Link) String() string { return string(l) }

// Links is a collection of navigation references. It marshals as an array
// of {"@odata.id": ...} objects, the form Redfish uses for Members and for
// Links.ManagerForServers.
type Links []Link

// ToStrings flattens the collection, mirroring schemas.Links.ToStrings.
func (l Links) ToStrings() []string {
	out := make([]string, 0, len(l))
	for _, link := range l {
		out = append(out, link.String())
	}
	return out
}

// Canonical resource paths. Every one derives from schemas.DefaultServiceRoot
// so the service root is the single source of truth for the prefix.
//
// DefaultServiceRoot carries a trailing slash ("/redfish/v1/") — that is the
// DMTF-canonical spelling of the root and what gofish requests during Login.
// Child resources hang off it without one, per the same convention.
const (
	ServiceRootPath = schemas.DefaultServiceRoot
	metadataPath    = schemas.DefaultServiceRoot + "$metadata"

	systemsPath     = schemas.DefaultServiceRoot + "Systems"
	systemPath      = systemsPath + "/1"
	systemResetPath = systemPath + "/Actions/ComputerSystem.Reset"
	biosPath        = systemPath + "/Bios"

	memoryPath     = systemPath + "/Memory"
	processorsPath = systemPath + "/Processors"
	processorPath  = processorsPath + "/" + processorID

	// Storage "1" is the host's own storage (host-reported drives); "BMC"
	// is what the BMC's USB gadget presents to it. See storage.go.
	storageRootPath   = systemPath + "/Storage"
	storagePath       = storageRootPath + "/" + storageID
	drivesPath        = storagePath + "/Drives"
	gadgetStoragePath = storageRootPath + "/" + gadgetStorageID
	gadgetDrivesPath  = gadgetStoragePath + "/Drives"

	// Host-reported boot options and secure-boot state (hostreports.go).
	bootOptionsPath = systemPath + "/BootOptions"
	secureBootPath  = systemPath + "/SecureBoot"

	ethernetInterfacesPath = systemPath + "/EthernetInterfaces"

	chassisItemPath    = chassisPath + "/1"
	chassisThermalPath = chassisItemPath + "/Thermal"

	biosSettingsPath = biosPath + "/Settings"
	// The registry lives under the Bios resource because EDK2's
	// BiosAttributeRegistryDxe builds its URI as
	// <parent of Bios>/<AttributeRegistry property value>.
	biosRegistryPath = biosPath + "/" + biosRegistryName

	registriesPath   = schemas.DefaultServiceRoot + "Registries"
	registryFilePath = registriesPath + "/" + biosRegistryName

	managersPath = schemas.DefaultServiceRoot + "Managers"
	managerPath  = managersPath + "/1"

	serialInterfacesPath = managerPath + "/SerialInterfaces"
	serialInterfacePath  = serialInterfacesPath + "/1"

	networkInterfacesPath = managerPath + "/NetworkInterfaces"

	virtualMediaPath   = managerPath + "/VirtualMedia"
	virtualMediaCDPath = virtualMediaPath + "/CD"

	dellAttributesPath = managerPath + "/Oem/Dell/DellAttributes/iDRAC.Embedded.1"

	chassisPath = schemas.DefaultServiceRoot + "Chassis"

	sessionServicePath = schemas.DefaultServiceRoot + "SessionService"
	sessionsPath       = sessionServicePath + "/Sessions"

	updateServicePath     = schemas.DefaultServiceRoot + "UpdateService"
	firmwareInventoryPath = updateServicePath + "/FirmwareInventory"
	firmwareBIOSPath      = firmwareInventoryPath + "/BIOS"
	simpleUpdatePath      = updateServicePath + "/Actions/UpdateService.SimpleUpdate"
	httpPushURIPath       = updateServicePath + "/update"
)

// odataTypeKey is the @odata.type property name. Typed resources get it from
// their struct tag; the free-form Oem maps have to spell it out.
const odataTypeKey = "@odata.type"

// context builds an @odata.context value for the given schema fragment.
func context(fragment string) string { return metadataPath + "#" + fragment }
