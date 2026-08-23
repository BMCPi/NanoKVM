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
const (
	odataTypeComputerSystem    = "#ComputerSystem.v1_13_0.ComputerSystem"
	odataTypeBios              = "#Bios.v1_1_0.Bios"
	odataTypeAttributeRegistry = "#AttributeRegistry.v1_3_6.AttributeRegistry"
	odataTypeBootOption        = "#BootOption.v1_0_4.BootOption"
	odataTypeSecureBoot        = "#SecureBoot.v1_1_0.SecureBoot"
	odataTypeMemory            = "#Memory.v1_7_1.Memory"
)
