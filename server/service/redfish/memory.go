package redfish

// memory.go exposes the SMBIOS type 16/17 tables as the standard Redfish
// Memory collection (/redfish/v1/Systems/1/Memory), one Memory resource per
// installed module. This is the DSP2046 home for the per-module detail
// (device type, speed, part/serial numbers) that previously sat under
// Oem.NanoKVM on the ComputerSystem — MemorySummary carries only the total.
//
// JetKVM's service exposes the same collection but is fed by host POSTs
// (its host has no out-of-band SMBIOS channel). Here the host's firmware
// already writes its SMBIOS tables into the shared EEPROM, so the members
// are served read-only from that store; module Ids follow the same
// DeviceLocator-derived convention JetKVM uses.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/server/service/smbios"
)

// Memory is the Redfish Memory resource (DSP2046 §6.47), populated from an
// SMBIOS type-17 Memory Device.
type Memory struct {
	Resource
	MemoryType        schemas.MemoryType       `json:"MemoryType,omitempty"`
	MemoryDeviceType  schemas.MemoryDeviceType `json:"MemoryDeviceType,omitempty"`
	CapacityMiB       *int                     `json:"CapacityMiB,omitempty"`
	OperatingSpeedMhz *int                     `json:"OperatingSpeedMhz,omitempty"`
	AllowedSpeedsMHz  []int                    `json:"AllowedSpeedsMHz,omitempty"`
	DataWidthBits     *int                     `json:"DataWidthBits,omitempty"`
	BusWidthBits      *int                     `json:"BusWidthBits,omitempty"`
	Manufacturer      string                   `json:"Manufacturer,omitempty"`
	PartNumber        string                   `json:"PartNumber,omitempty"`
	SerialNumber      string                   `json:"SerialNumber,omitempty"`
	DeviceLocator     string                   `json:"DeviceLocator,omitempty"`
	// ErrorCorrection is array-level in SMBIOS (type 16) but a per-module
	// property in Redfish; every module reports its array's value.
	ErrorCorrection schemas.ErrorCorrection `json:"ErrorCorrection,omitempty"`
	Status          *Status                 `json:"Status,omitempty"`
}

// memoryModules returns the installed modules and the array-level ECC mode,
// or nil when no SMBIOS tables are available (the host has not booted yet).
func memoryModules() ([]smbios.MemoryModule, string) {
	info, err := smbios.GetStore().Load()
	if err != nil || info == nil {
		return nil, ""
	}
	return info.Memory, info.MemoryErrorCorrection
}

// memoryID derives a URL-safe member Id, preferring the SMBIOS device
// locator (JetKVM's convention) over a positional name.
func memoryID(i int, m smbios.MemoryModule) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return -1
		}
	}, m.Locator)
	if id == "" {
		id = fmt.Sprintf("DIMM%d", i)
	}
	return id
}

// memoryDeviceType maps the SMBIOS type-17 device-type name onto the Redfish
// MemoryDeviceType enum; unknown names are omitted rather than guessed.
func memoryDeviceType(t string) schemas.MemoryDeviceType {
	switch t {
	case "DDR", "DDR2", "DDR3", "DDR4", "DDR5":
		return schemas.MemoryDeviceType(t)
	case "LPDDR", "LPDDR2", "LPDDR3", "LPDDR4", "LPDDR5":
		// Redfish spells the low-power variants with an _SDRAM suffix.
		return schemas.MemoryDeviceType(t + "_SDRAM")
	default:
		return ""
	}
}

// memoryErrorCorrection maps the SMBIOS type-16 ECC name onto the Redfish
// ErrorCorrection enum.
func memoryErrorCorrection(s string) schemas.ErrorCorrection {
	switch s {
	case "None":
		return schemas.NoECCErrorCorrection
	case "Parity":
		return schemas.AddressParityErrorCorrection
	case "Single-bit ECC":
		return schemas.SingleBitECCErrorCorrection
	case "Multi-bit ECC":
		return schemas.MultiBitECCErrorCorrection
	default:
		return ""
	}
}

// memoryResource maps one SMBIOS module onto a Memory resource.
func memoryResource(id string, m smbios.MemoryModule, ecc string) Memory {
	res := Memory{
		Resource: Resource{
			ODataType:    "#Memory.v1_16_0.Memory",
			ODataID:      memoryPath + "/" + id,
			ODataContext: context("Memory.Memory"),
			ID:           id,
			Name:         "Memory Module",
		},
		MemoryType:       schemas.DRAMMemoryType,
		MemoryDeviceType: memoryDeviceType(m.Type),
		Manufacturer:     m.Manufacturer,
		PartNumber:       m.PartNumber,
		SerialNumber:     m.SerialNumber,
		DeviceLocator:    m.Locator,
		ErrorCorrection:  memoryErrorCorrection(ecc),
		Status:           &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
	if m.SizeMB > 0 {
		size := m.SizeMB
		res.CapacityMiB = &size
	}
	// SMBIOS reports MT/s; Redfish's *Mhz members conventionally carry the
	// transfer rate (a "3200 MHz" DDR4 module), matching vendor BMCs.
	if m.ConfiguredSpeedMTs > 0 {
		sp := m.ConfiguredSpeedMTs
		res.OperatingSpeedMhz = &sp
	} else if m.SpeedMTs > 0 {
		sp := m.SpeedMTs
		res.OperatingSpeedMhz = &sp
	}
	if m.SpeedMTs > 0 {
		res.AllowedSpeedsMHz = []int{m.SpeedMTs}
	}
	if m.DataWidthBits > 0 {
		w := m.DataWidthBits
		res.DataWidthBits = &w
	}
	if m.TotalWidthBits > 0 {
		w := m.TotalWidthBits
		res.BusWidthBits = &w
	}
	return res
}

func (s *Service) GetMemoryCollection(c *gin.Context) {
	modules, _ := memoryModules()
	links := make([]Link, 0, len(modules))
	for i, m := range modules {
		links = append(links, Link(memoryPath+"/"+memoryID(i, m)))
	}
	c.JSON(http.StatusOK, newCollection(
		"MemoryCollection", "Memory Module Collection", memoryPath, links...))
}

func (s *Service) GetMemoryModule(c *gin.Context) {
	want := c.Param("module")
	modules, ecc := memoryModules()
	for i, m := range modules {
		if memoryID(i, m) == want {
			c.JSON(http.StatusOK, memoryResource(want, m, ecc))
			return
		}
	}
	redfishErrorResponse(c, http.StatusNotFound, "memory module not found: "+want)
}
