package redfish

// schema_versions.go is the single place the schema versions this BMC
// advertises are written down.
//
// They are not free choices. The managed host runs EDK2's RedfishClientPkg,
// and each of its feature drivers is compiled against one exact schema
// version — the same version its HII questions are tagged with
// (x-UEFI-redfish-<Schema>.<version>). The RPi5 firmware builds:
//
//	RedfishClientPkg/Features/ComputerSystem/v1_5_0
//	RedfishClientPkg/Features/Bios/v1_0_9
//	RedfishClientPkg/Features/BiosAttributeRegistry/v1_3_6
//	RedfishClientPkg/Features/BootOption/v1_0_4
//	RedfishClientPkg/Features/SecureBoot/v1_1_0
//	RedfishClientPkg/Features/Memory/V1_7_1
//
// Advertising a different version is not an outright failure: the client
// rewrites @odata.type to its own when PcdRedfishCompatibleSchemaSupport is
// set, and it is set by default. But that is a fallback path, taken silently
// — nothing on the BMC side reports the mismatch, and the client's generated
// C structure has no field for a property from a version it was not built
// against, so anything extra is dropped on the floor. Matching exactly keeps
// the client on its normal path.
//
// Where the host publishes a resource itself (memory modules, boot options,
// the attribute registry) it supplies its own @odata.type and that one wins;
// these are the types used for the documents this BMC renders on its own.
// redfishProtocolVersion is the value the service root reports as
// RedfishVersion. It is the *protocol* version from DSP0266's Protocol
// Version clause — not the ServiceRoot schema version, which is a separate
// number (odataTypeServiceRoot below).
//
// 1.13.0 is the lowest protocol version consistent with what this BMC
// actually serves: odataTypeComputerSystem is pinned to v1_13_0, and
// BootProgress — which the host firmware PATCHes and systems.go publishes —
// is that version's addition. Clients gate on this. bmclib, the library
// behind Tinkerbell's Rufio, refuses to read BootProgress from a service
// reporting less than 1.13.0, so the "1.0.0" this replaced was silently
// switching off a feature we implement.
//
// Protocol minor versions are additive, so under-claiming costs features
// while over-claiming promises behaviour we do not have: raise this only to
// match a capability actually added here. Not implementing
// ProtocolFeaturesSupported (protocol 1.6+) is not a reason to claim less —
// that property is optional, and its absence correctly means "no query
// parameters supported".
const redfishProtocolVersion = "1.13.0"

const (
	// The service root's own schema version, distinct from the protocol
	// version above.
	odataTypeServiceRoot = "#ServiceRoot.v1_9_0.ServiceRoot"

	odataTypeComputerSystem    = "#ComputerSystem.v1_13_0.ComputerSystem"
	odataTypeBios              = "#Bios.v1_1_0.Bios"
	odataTypeAttributeRegistry = "#AttributeRegistry.v1_3_6.AttributeRegistry"
	odataTypeBootOption        = "#BootOption.v1_0_4.BootOption"
	odataTypeSecureBoot        = "#SecureBoot.v1_1_0.SecureBoot"
	odataTypeMemory            = "#Memory.v1_7_1.Memory"

	// Sensor has no counterpart in RedfishClientPkg — the host does not
	// consume it, this BMC only publishes it — so it is pinned to the
	// schema the resource is actually written against rather than to a
	// feature driver's version.
	odataTypeSensor = "#Sensor.v1_2_0.Sensor"
)
