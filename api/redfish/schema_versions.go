package redfish

import "github.com/pi-bmc/nanokvm-app/pkg/protocol/identity"

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
// RedfishVersion. The value and the reasoning behind it (why 1.13.0, and why
// it must not be lowered) live in pkg/identity.RedfishProtocolVersion —
// pkg/discovery advertises the same version over SSDP/DNS-SD, and a pkg may
// not import api.
const redfishProtocolVersion = identity.RedfishProtocolVersion

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
	// Hand-built by RpiRedfishSyncDxe (no converter lib), so this version is
	// pinned here rather than in the feature-driver table.
	odataTypeEthernetInterface = "#EthernetInterface.v1_8_0.EthernetInterface"

	// Sensor has no counterpart in RedfishClientPkg — the host does not
	// consume it, this BMC only publishes it — so it is pinned to the
	// schema the resource is actually written against rather than to a
	// feature driver's version.
	odataTypeSensor = "#Sensor.v1_2_0.Sensor"

	// TaskService/Task are operator-only (the host client never polls tasks);
	// pinned to published DMTF versions, Task ≥ v1_4_0 for PercentComplete.
	odataTypeTaskService = "#TaskService.v1_2_0.TaskService"
	// Task v1_4_3 is the published errata of the first PercentComplete-bearing
	// Task schema.
	odataTypeTask = "#Task.v1_4_3.Task"
)

// Schema names, the unversioned counterpart to the odataTypeXxx constants
// above. Used wherever the bare DMTF term appears rather than the full
// "#Type.vX_Y_Z.Type" form: a resource's own Id (this BMC names each
// singleton resource after its schema), the $metadata schema list
// (metadata.go), and the registry-name vocabulary in bios.go.
const (
	schemaNameServiceRoot       = "ServiceRoot"
	schemaNameBios              = "Bios"
	schemaNameSecureBoot        = "SecureBoot"
	schemaNameAttributeRegistry = "AttributeRegistry"
)
