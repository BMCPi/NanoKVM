package redfish

// processors.go exposes the host CPU as the standard Redfish Processors
// collection (/redfish/v1/Systems/1/Processors). The Processor resource is
// the DSP2046 home for the manufacturer, part number and max speed that
// ProcessorSummary has no members for — they previously sat under
// Oem.NanoKVM on the ComputerSystem. SMBIOS type 4 is the source, with the
// U-Boot env's "cpu" as the pre-first-boot fallback; the managed Pi is a
// single-socket part, so the collection has exactly one member.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"
)

// processorID is the single member's Id ("CPU1", the common BMC convention).
const processorID = "CPU1"

// Processor is the Redfish Processor resource (DSP2046 §6.60).
type Processor struct {
	Resource
	ProcessorType         schemas.ProcessorType         `json:"ProcessorType,omitempty"`
	ProcessorArchitecture schemas.ProcessorArchitecture `json:"ProcessorArchitecture,omitempty"`
	InstructionSet        schemas.InstructionSet        `json:"InstructionSet,omitempty"`
	Manufacturer          string                        `json:"Manufacturer,omitempty"`
	Model                 string                        `json:"Model,omitempty"`
	PartNumber            string                        `json:"PartNumber,omitempty"`
	MaxSpeedMHz           *int                          `json:"MaxSpeedMHz,omitempty"`
	TotalCores            *uint                         `json:"TotalCores,omitempty"`
	TotalThreads          *uint                         `json:"TotalThreads,omitempty"`
	Status                *Status                       `json:"Status,omitempty"`
}

// processorResource assembles the member from SMBIOS, falling back to the
// env's cpu string. Returns false when neither source knows anything.
func processorResource(fw *firmware.Controller) (Processor, bool) {
	res := Processor{
		Resource: Resource{
			ODataType:    "#Processor.v1_16_0.Processor",
			ODataID:      processorPath,
			ODataContext: context("Processor.Processor"),
			ID:           processorID,
			Name:         "Processor",
		},
		ProcessorType: schemas.CPUProcessorType,
		// The managed host is by design an aarch64 Raspberry Pi (see the
		// build's rpi multiconfig); SMBIOS type 4 carries no usable
		// architecture field to derive this from.
		ProcessorArchitecture: schemas.ARMProcessorArchitecture,
		InstructionSet:        schemas.ARMA64InstructionSet,
		Status:                &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}

	if info, err := smbios.GetStore().Load(); err == nil && info != nil &&
		(info.CPUVersion != "" || info.CPUCores > 0) {
		res.Manufacturer = info.CPUManufacturer
		res.Model = info.CPUVersion
		res.PartNumber = info.CPUPartNumber
		if info.CPUMaxSpeedMHz > 0 {
			mhz := info.CPUMaxSpeedMHz
			res.MaxSpeedMHz = &mhz
		}
		if info.CPUCores > 0 {
			res.TotalCores = uptr(info.CPUCores)
		}
		if info.CPUThreads > 0 {
			res.TotalThreads = uptr(info.CPUThreads)
		}
		return res, true
	}

	if inv, err := fw.GetInventory(); err == nil {
		if cpu := inv["cpu"]; cpu != "" {
			res.Model = cpu
			return res, true
		}
	}
	return res, false
}

func (s *Service) GetProcessorCollection(c *gin.Context) {
	var links []Link
	if _, ok := processorResource(s.Firmware); ok {
		links = append(links, Link(processorPath))
	}
	c.JSON(http.StatusOK, newCollection(
		"ProcessorCollection", "Processor Collection", processorsPath, links...))
}

func (s *Service) GetProcessor(c *gin.Context) {
	if c.Param("processor") != processorID {
		redfishErrorResponse(c, http.StatusNotFound, "processor not found")
		return
	}
	res, ok := processorResource(s.Firmware)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "no processor inventory available")
		return
	}
	c.JSON(http.StatusOK, res)
}
