package redfish

// processors.go serves the Processors collection
// (/redfish/v1/Systems/1/Processors). With the EEPROM inventory gone the BMC
// knows only what the platform design guarantees: the managed host is a
// single-socket aarch64 part. The member is that minimal static fact; model
// and speed detail would have to arrive as a host report to be claimed.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// processorID is the single member's Id ("CPU1", the common BMC convention).
const processorID = "CPU1"

// Processor is the Redfish Processor resource (DSP2046 §6.60).
type Processor struct {
	Resource
	ProcessorType         schemas.ProcessorType         `json:"ProcessorType,omitempty"`
	ProcessorArchitecture schemas.ProcessorArchitecture `json:"ProcessorArchitecture,omitempty"`
	InstructionSet        schemas.InstructionSet        `json:"InstructionSet,omitempty"`
	Status                *Status                       `json:"Status,omitempty"`
}

func processorResource() Processor {
	return Processor{
		Resource: Resource{
			ODataType:    "#Processor.v1_16_0.Processor",
			ODataID:      processorPath,
			ODataContext: context("Processor.Processor"),
			ID:           processorID,
			Name:         "Processor",
		},
		ProcessorType: schemas.CPUProcessorType,
		// The managed host is by design an aarch64 Raspberry Pi (see the
		// build's rpi multiconfig).
		ProcessorArchitecture: schemas.ARMProcessorArchitecture,
		InstructionSet:        schemas.ARMA64InstructionSet,
		Status:                &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
}

func (s *Service) GetProcessorCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ProcessorCollection", "Processor Collection", processorsPath,
		Link(processorPath)))
}

func (s *Service) GetProcessor(c *gin.Context) {
	if c.Param("processor") != processorID {
		redfishErrorResponse(c, http.StatusNotFound, "processor not found")
		return
	}
	c.JSON(http.StatusOK, processorResource())
}
