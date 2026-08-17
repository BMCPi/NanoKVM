package redfish

// memory.go serves the Memory collection (/redfish/v1/Systems/1/Memory) from
// what the managed host reported over the host interface. The BMC has no
// out-of-band channel to the host's DRAM: the host's firmware enumerates its
// modules and POSTs one Memory resource per module (hostreports.go); these
// handlers only render what was stored. Empty until the host first boots.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Service) GetMemoryCollection(c *gin.Context) {
	ids := hostCollectionIDs(memoryOf)
	links := make([]Link, 0, len(ids))
	for _, id := range ids {
		links = append(links, Link(memoryPath+"/"+id))
	}
	writeHostResource(c, hostView(newCollection(
		"MemoryCollection", "Memory Module Collection", memoryPath, links...)))
}

func (s *Service) GetMemoryModule(c *gin.Context) {
	id := c.Param("module")
	stored, ok := hostCollectionGet(memoryOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "memory module not found: "+id)
		return
	}
	writeHostResource(c, renderHostMember(stored, memoryPath+"/"+id, id,
		"#Memory.v1_16_0.Memory", "Memory.Memory", "Memory Module"))
}
