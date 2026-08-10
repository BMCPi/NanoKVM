package redfish

// resources.go defines the response bodies we serve. The enums come from
// gofish (schemas.BootSource, schemas.PowerState, ...) so an invalid value
// is a compile error rather than a typo a client discovers at runtime; the
// struct shapes are ours, because gofish's cannot serialize (see odata.go).
//
// Every optional property is omitempty: Redfish enums have no valid empty
// member, so emitting "PowerState": "" is worse than omitting the property.

import (
	"github.com/stmcginnis/gofish/schemas"
)

// Oem is a vendor-namespaced extension block (DSP0266 §6.4.13). Free-form
// by definition, so it stays a map.
type Oem map[string]any

// Resource is the OData preamble every Redfish resource carries.
//
// This is our analogue of schemas.Entity, which we can't embed: it models
// no @odata.type (a client never needs to write one), tags Id/Description
// without omitempty, and carries unexported client state that would leak
// into the output.
type Resource struct {
	ODataType    string `json:"@odata.type"`
	ODataID      string `json:"@odata.id"`
	ODataContext string `json:"@odata.context,omitempty"`
	ID           string `json:"Id,omitempty"`
	Name         string `json:"Name"`
	Description  string `json:"Description,omitempty"`
}

// Collection is a Redfish resource collection.
type Collection struct {
	Resource
	MembersCount int   `json:"Members@odata.count"`
	Members      Links `json:"Members"`
}

// newCollection builds a collection whose count always matches its members.
func newCollection(odataType, name, path string, members ...Link) Collection {
	if members == nil {
		members = Links{}
	}
	return Collection{
		Resource: Resource{
			ODataType:    "#" + odataType + "." + odataType,
			ODataID:      path,
			ODataContext: context(odataType + "." + odataType),
			Name:         name,
		},
		MembersCount: len(members),
		Members:      members,
	}
}

// Status is the standard resource status block.
type Status struct {
	State  schemas.State  `json:"State,omitempty"`
	Health schemas.Health `json:"Health,omitempty"`
}

// ---------------------------------------------------------------------------
// ServiceRoot

type ServiceRoot struct {
	Resource
	RedfishVersion string           `json:"RedfishVersion"`
	UUID           string           `json:"UUID,omitempty"`
	Systems        Link             `json:"Systems"`
	Managers       Link             `json:"Managers"`
	Chassis        Link             `json:"Chassis"`
	SessionService Link             `json:"SessionService"`
	UpdateService  Link             `json:"UpdateService"`
	Links          ServiceRootLinks `json:"Links"`
}

// ServiceRootLinks carries Links.Sessions, which is what gofish and other
// DMTF-conformant clients POST to during Login() — without it they fail
// with "unable to execute request, no target provided".
type ServiceRootLinks struct {
	Sessions Link `json:"Sessions"`
}

// ---------------------------------------------------------------------------
// ComputerSystem

type ComputerSystem struct {
	Resource
	SystemType   schemas.SystemType `json:"SystemType,omitempty"`
	PowerState   schemas.PowerState `json:"PowerState,omitempty"`
	Manufacturer string             `json:"Manufacturer,omitempty"`
	Model        string             `json:"Model,omitempty"`
	SubModel     string             `json:"SubModel,omitempty"`
	SKU          string             `json:"SKU,omitempty"`
	SerialNumber string             `json:"SerialNumber,omitempty"`
	PartNumber   string             `json:"PartNumber,omitempty"`
	UUID         string             `json:"UUID,omitempty"`
	// BiosVersion is the standard ComputerSystem property for firmware
	// version — the pre-migration code emitted "FirmwareVersion", which no
	// Redfish client reads because ComputerSystem does not define it.
	BiosVersion      string            `json:"BiosVersion,omitempty"`
	HostName         string            `json:"HostName,omitempty"`
	Status           *Status           `json:"Status,omitempty"`
	ProcessorSummary *ProcessorSummary `json:"ProcessorSummary,omitempty"`
	MemorySummary    *MemorySummary    `json:"MemorySummary,omitempty"`
	Boot             Boot              `json:"Boot"`
	Bios             *Link             `json:"Bios,omitempty"`
	// Sub-resource collections (see memory.go, processors.go, storage.go,
	// ethernet_interfaces.go) — the standard homes for the per-device
	// inventory that predates them under Oem.NanoKVM.
	Memory             *Link          `json:"Memory,omitempty"`
	Processors         *Link          `json:"Processors,omitempty"`
	Storage            *Link          `json:"Storage,omitempty"`
	EthernetInterfaces *Link          `json:"EthernetInterfaces,omitempty"`
	Actions            *SystemActions `json:"Actions,omitempty"`
	Links              *SystemLinks   `json:"Links,omitempty"`
	Oem                Oem            `json:"Oem,omitempty"`
}

// SystemLinks carries ComputerSystem navigation links. TrustedComponents is
// the standard place to advertise platform roots of trust; on the RPi 5 the
// rpi-eeprom bootloader is one, exposed with its firmware as a nested
// SoftwareInventory (see trusted_components.go). Chassis binds the system to
// the baseboard resource (chassis.go).
type SystemLinks struct {
	TrustedComponents Links `json:"TrustedComponents,omitempty"`
	Chassis           Links `json:"Chassis,omitempty"`
}

// ProcessorSummary mirrors the Redfish property set. Note there is no
// Manufacturer member — that lives on the individual Processor resource
// (processors.go), which carries the full SMBIOS type-4 detail.
type ProcessorSummary struct {
	Count                 *uint  `json:"Count,omitempty"`
	Model                 string `json:"Model,omitempty"`
	CoreCount             *uint  `json:"CoreCount,omitempty"`
	LogicalProcessorCount *uint  `json:"LogicalProcessorCount,omitempty"`
}

// MemorySummary mirrors the Redfish ComputerSystem.MemorySummary property set,
// derived from the SMBIOS type 16/17 tables. TotalSystemMemoryGiB is a pointer
// so an unknown total is omitted rather than reported as a misleading 0 GiB.
// Per-module detail (type, speed, part number) lives on the Memory collection
// members (memory.go).
type MemorySummary struct {
	TotalSystemMemoryGiB *float64                `json:"TotalSystemMemoryGiB,omitempty"`
	MemoryMirroring      schemas.MemoryMirroring `json:"MemoryMirroring,omitempty"`
	Status               *Status                 `json:"Status,omitempty"`
}

// Boot is the ComputerSystem boot settings block.
type Boot struct {
	BootSourceOverrideTarget  schemas.BootSource                `json:"BootSourceOverrideTarget"`
	BootSourceOverrideEnabled schemas.BootSourceOverrideEnabled `json:"BootSourceOverrideEnabled"`
	BootSourceOverrideMode    schemas.BootSourceOverrideMode    `json:"BootSourceOverrideMode,omitempty"`
	BootOrder                 []string                          `json:"BootOrder,omitempty"`

	AllowableTargets []schemas.BootSource                `json:"BootSourceOverrideTarget@Redfish.AllowableValues,omitempty"`
	AllowableEnabled []schemas.BootSourceOverrideEnabled `json:"BootSourceOverrideEnabled@Redfish.AllowableValues,omitempty"`
	AllowableModes   []schemas.BootSourceOverrideMode    `json:"BootSourceOverrideMode@Redfish.AllowableValues,omitempty"`
}

type SystemActions struct {
	Reset ResetAction `json:"#ComputerSystem.Reset"`
}

type ResetAction struct {
	Target            string              `json:"target"`
	AllowableResetVal []schemas.ResetType `json:"ResetType@Redfish.AllowableValues,omitempty"`
}

// ---------------------------------------------------------------------------
// Manager

type Manager struct {
	Resource
	ManagerType      schemas.ManagerType `json:"ManagerType"`
	FirmwareVersion  string              `json:"FirmwareVersion,omitempty"`
	Status           *Status             `json:"Status,omitempty"`
	SerialInterfaces Link                `json:"SerialInterfaces"`
	VirtualMedia     Link                `json:"VirtualMedia"`
	// NetworkInterfaces is advertised for client compatibility even though
	// the collection is not implemented; this predates the migration.
	NetworkInterfaces Link         `json:"NetworkInterfaces"`
	Links             ManagerLinks `json:"Links"`
	// Empty Oem/Actions.Oem keep gofish's dell.Manager() unmarshal from
	// erroring on "unexpected end of JSON input" — the wrapper
	// json.Unmarshal's the raw bytes of each field and aborts when they're
	// absent rather than `{}`. So these are deliberately not omitempty.
	Oem     Oem `json:"Oem"`
	Actions Oem `json:"Actions"`
}

// ManagerLinks binds this BMC to the system(s) it manages. Standards-based
// clients (Dell terraform provider, bmclib) resolve system_id from
// ManagerForServers when invoking actions that target a ComputerSystem.
//
// Links.Oem.Dell.DellAttributes points the Dell terraform provider at our
// fake iDRAC AttributeRegistry. The provider hard-codes a Dell.Manager()
// unmarshal whose generation check (sub-17G vs 17G+) gates the
// boot-source-override code path; we report 14G so the standard PATCH
// /Systems/1 path is used.
type ManagerLinks struct {
	ManagerForServers Links `json:"ManagerForServers"`
	Oem               Oem   `json:"Oem,omitempty"`
}

// ---------------------------------------------------------------------------
// SerialInterface

// SerialInterface reports the console UART settings.
//
// BitRate, DataBits and StopBits are *string* enums in the Redfish schema,
// not numbers. The pre-migration code emitted them as JSON numbers, which
// a conformant client cannot read: gofish unmarshals into schemas.BitRate
// (a string) and fails with "cannot unmarshal number". Using gofish's own
// types here makes that class of mistake unrepresentable.
type SerialInterface struct {
	Resource
	InterfaceEnabled bool                               `json:"InterfaceEnabled"`
	BitRate          schemas.BitRate                    `json:"BitRate,omitempty"`
	Parity           schemas.Parity                     `json:"Parity,omitempty"`
	DataBits         schemas.DataBits                   `json:"DataBits,omitempty"`
	StopBits         schemas.StopBits                   `json:"StopBits,omitempty"`
	FlowControl      schemas.SerialInferfaceFlowControl `json:"FlowControl,omitempty"`
	ConnectorType    schemas.ConnectorType              `json:"ConnectorType,omitempty"`
	SignalType       schemas.SignalType                 `json:"SignalType,omitempty"`
}

// ---------------------------------------------------------------------------
// SessionService / Session

type SessionService struct {
	Resource
	ServiceEnabled bool `json:"ServiceEnabled"`
	Sessions       Link `json:"Sessions"`
}

type Session struct {
	Resource
	UserName string `json:"UserName,omitempty"`
}

// ---------------------------------------------------------------------------
// UpdateService

type UpdateService struct {
	Resource
	ServiceEnabled    bool                 `json:"ServiceEnabled"`
	FirmwareInventory Link                 `json:"FirmwareInventory"`
	Actions           UpdateServiceActions `json:"Actions"`
}

type UpdateServiceActions struct {
	SimpleUpdate SimpleUpdateAction `json:"#UpdateService.SimpleUpdate"`
	StartUpdate  ActionTarget       `json:"#UpdateService.StartUpdate"`
}

type ActionTarget struct {
	Target string `json:"target"`
}

type SimpleUpdateAction struct {
	Target                    string   `json:"target"`
	AllowableTransferProtocol []string `json:"TransferProtocol@Redfish.AllowableValues,omitempty"`
}

type SoftwareInventory struct {
	Resource
	SoftwareID string `json:"SoftwareId,omitempty"`
	Version    string `json:"Version,omitempty"`
	// Manufacturer is the firmware vendor (SMBIOS type-0 Vendor for the
	// BIOS/U-Boot image).
	Manufacturer string `json:"Manufacturer,omitempty"`
	// ReleaseDate is the firmware's release/production date (ISO 8601). For
	// the bootloader it carries the EEPROM flash time from
	// BootloaderUpdateTimestamp.
	ReleaseDate   string                `json:"ReleaseDate,omitempty"`
	VersionScheme schemas.VersionScheme `json:"VersionScheme,omitempty"`
	Updateable    bool                  `json:"Updateable"`
	Status        *Status               `json:"Status,omitempty"`
	Oem           Oem                   `json:"Oem,omitempty"`
}

// ---------------------------------------------------------------------------
// TrustedComponent

// TrustedComponent models a platform root of trust and links to the firmware
// running on it. The rpi-eeprom bootloader is the RPi 5 RoT: it is the
// first-stage, secure-boot-capable loader integrated into the SoC's SPI flash.
type TrustedComponent struct {
	Resource
	TrustedComponentType schemas.TrustedComponentType `json:"TrustedComponentType,omitempty"`
	Manufacturer         string                       `json:"Manufacturer,omitempty"`
	Model                string                       `json:"Model,omitempty"`
	FirmwareVersion      string                       `json:"FirmwareVersion,omitempty"`
	SerialNumber         string                       `json:"SerialNumber,omitempty"`
	Status               *Status                      `json:"Status,omitempty"`
	Links                *TrustedComponentLinks       `json:"Links,omitempty"`
	Oem                  Oem                          `json:"Oem,omitempty"`
}

// TrustedComponentLinks references the component's firmware images and the
// resource it is integrated into (the ComputerSystem).
type TrustedComponentLinks struct {
	ActiveSoftwareImage *Link `json:"ActiveSoftwareImage,omitempty"`
	SoftwareImages      Links `json:"SoftwareImages,omitempty"`
	IntegratedInto      *Link `json:"IntegratedInto,omitempty"`
}

// ---------------------------------------------------------------------------
// Bios

type Bios struct {
	Resource
	AttributeRegistry string            `json:"AttributeRegistry,omitempty"`
	BiosVersion       string            `json:"BiosVersion,omitempty"`
	Attributes        map[string]string `json:"Attributes"`

	Settings *SettingsAnnotation `json:"@Redfish.Settings,omitempty"`

	// Pending is not a Redfish property; it predates this migration and is
	// kept because the local UI reads it. Conformant clients ignore it.
	Pending *bool `json:"Pending,omitempty"`

	Actions      map[string]ActionTarget `json:"Actions,omitempty"`
	Links        Oem                     `json:"Links,omitempty"`
	Oem          Oem                     `json:"Oem,omitempty"`
	ExtendedInfo []MessageInfo           `json:"@Message.ExtendedInfo,omitempty"`
}

// SettingsAnnotation is the DSP2046 @Redfish.Settings block pointing clients
// at the SettingsObject used to stage changes.
type SettingsAnnotation struct {
	ODataType           string   `json:"@odata.type"`
	SettingsObject      Link     `json:"SettingsObject"`
	SupportedApplyTimes []string `json:"SupportedApplyTimes,omitempty"`
}

// MessageInfo is one entry of an @Message.ExtendedInfo array.
type MessageInfo struct {
	MessageID string `json:"MessageId"`
	Message   string `json:"Message"`
	Severity  string `json:"Severity,omitempty"`
}

// ---------------------------------------------------------------------------
// VirtualMedia

type VirtualMedia struct {
	Resource
	MediaTypes     []schemas.VirtualMediaType `json:"MediaTypes,omitempty"`
	MediaType      schemas.VirtualMediaType   `json:"MediaType,omitempty"`
	ConnectedVia   schemas.ConnectedVia       `json:"ConnectedVia,omitempty"`
	Inserted       bool                       `json:"Inserted"`
	WriteProtected bool                       `json:"WriteProtected"`
	InsertedMedia  *InsertedMedia             `json:"InsertedMedia,omitempty"`

	// Image / TransferMethod / TransferProtocolType echo back the most
	// recent successful InsertMedia. The Dell terraform provider diffs them
	// against config on refresh and raises "inconsistent result after apply"
	// if a value it set doesn't come back — so they must survive a
	// round-trip. omitempty only drops them when nothing is inserted, which
	// no terraform config covers.
	Image                string                                   `json:"Image,omitempty"`
	TransferMethod       schemas.TransferMethod                   `json:"TransferMethod,omitempty"`
	TransferProtocolType schemas.VirtualMediaTransferProtocolType `json:"TransferProtocolType,omitempty"`

	Links   VirtualMediaLinks   `json:"Links"`
	Actions VirtualMediaActions `json:"Actions"`
}

type InsertedMedia struct {
	ImageName     string `json:"ImageName,omitempty"`
	CapacityBytes int64  `json:"CapacityBytes,omitempty"`
}

type VirtualMediaLinks struct {
	Systems Links `json:"Systems"`
}

type VirtualMediaActions struct {
	InsertMedia ActionTarget `json:"#VirtualMedia.InsertMedia"`
	EjectMedia  ActionTarget `json:"#VirtualMedia.EjectMedia"`
}

// ---------------------------------------------------------------------------
// Message

// Message is the standalone action-response body (#Message.v1_1_0.Message).
type Message struct {
	ODataType string `json:"@odata.type"`
	MessageID string `json:"MessageId"`
	Message   string `json:"Message"`
	Severity  string `json:"Severity,omitempty"`
}
