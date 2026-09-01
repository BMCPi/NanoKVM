package redfish

// processors.go serves the Processors collection
// (/redfish/v1/Systems/1/Processors). The BMC is board-agnostic: it does not
// know the managed host's architecture until the host reports itself, so the
// pre-sync placeholder claims only that a CPU exists, never an architecture
// or instruction set — a host report is the only source for those.

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

// processorResource is the BMC's own description of the managed host's CPU,
// served when the host has not published anything. It is deliberately
// architecture-neutral: ProcessorArchitecture and InstructionSet are left
// unset (omitempty) rather than guessing, because this BMC now targets more
// than one board family. A host that enumerates its own processors overrides
// it entirely (see GetProcessor).
func processorResource() Processor {
	return Processor{
		Resource: Resource{
			ODataType:    odataTypeProcessor,
			ODataID:      processorPath,
			ODataContext: odataContext("Processor.Processor"),
			ID:           processorID,
			Name:         "Processor",
		},
		ProcessorType: schemas.CPUProcessorType,
		Status:        &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
	}
}

// GetProcessorCollection lists what the host reported. Until it reports
// anything the collection holds the BMC's own placeholder, CPU1: a
// single-socket system has a processor by construction, so a read before the
// host has ever booted can still answer something true (just not which
// architecture) rather than an empty list.
//
// The first host report replaces that placeholder outright. It is the host's
// enumeration that is authoritative once it exists — leaving CPU1 alongside it
// would report a socket the host did not find.
func (s *Service) GetProcessorCollection(c *gin.Context) {
	ids := hostCollectionIDs(processorsOf)
	if len(ids) == 0 {
		writeHostResource(c, hostView(newCollection(
			"ProcessorCollection", "Processor Collection", processorsPath,
			Link(processorPath))))
		return
	}

	links := make([]Link, 0, len(ids))
	for _, id := range ids {
		links = append(links, Link(processorsPath+"/"+id))
	}
	writeHostResource(c, hostView(newCollection(
		"ProcessorCollection", "Processor Collection", processorsPath, links...)))
}

// GetProcessor serves a host-reported member, or the built-in placeholder while
// the host has reported none.
func (s *Service) GetProcessor(c *gin.Context) {
	id := c.Param("processor")

	if stored, ok := hostCollectionGet(processorsOf, id); ok {
		writeHostResource(c, renderHostMember(stored, processorsPath+"/"+id, id,
			odataTypeProcessor, "Processor.Processor", "Processor"))
		return
	}

	// The placeholder answers for its own id only while the collection is
	// empty. Once the host has enumerated, an id it did not report is a real
	// 404 — otherwise a CPU the host does not have would keep being served
	// from a constant.
	if id == processorID && len(hostCollectionIDs(processorsOf)) == 0 {
		c.JSON(http.StatusOK, processorResource())
		return
	}

	redfishErrorResponse(c, http.StatusNotFound, "processor not found: "+id)
}
