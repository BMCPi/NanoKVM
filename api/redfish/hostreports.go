package redfish

// hostreports.go serves the resources the managed host's firmware owns:
// BootOptions, Memory, Drives, Bios attributes and SecureBoot. The host is a
// Redfish client on the USB host interface (DSP0270) and PUSHES this
// inventory up to the BMC; operators only read it. Writes are therefore
// gated on hostWritable — except PATCH /Bios/Settings, which is the
// operator's staging surface and uses normal authentication.
//
// Every response body here is written with an exact "application/json"
// content type (EDK2's Redfish client rejects "; charset=utf-8") and carries
// an @odata.etag: EDK2 round-trips ETags as If-Match, and a service without
// them makes that protection vacuous.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// --- host-owned collection storage -------------------------------------------

// hostCollectionKind selects one of the host-owned collections under the
// state lock. The selector runs with host.mu held.
type hostCollectionKind func(*hostState) map[string]map[string]any

var (
	bootOptionsOf = func(h *hostState) map[string]map[string]any { return h.BootOptions }
	memoryOf      = func(h *hostState) map[string]map[string]any { return h.Memory }
	processorsOf  = func(h *hostState) map[string]map[string]any { return h.Processors }
	drivesOf      = func(h *hostState) map[string]map[string]any { return h.Drives }
	firmwareOf    = func(h *hostState) map[string]map[string]any { return h.Firmware }
	ethernetOf    = func(h *hostState) map[string]map[string]any { return h.Ethernet }
)

// hostCollectionIDs returns the member ids in stable (sorted) order.
func hostCollectionIDs(kind hostCollectionKind) []string {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return slices.Sorted(maps.Keys(kind(host)))
}

// hostCollectionGet returns a copy of one member.
func hostCollectionGet(kind hostCollectionKind, id string) (map[string]any, bool) {
	host.mu.RLock()
	defer host.mu.RUnlock()
	m, ok := kind(host)[id]
	if !ok {
		return nil, false
	}
	return copyAnyMap(m), true
}

// hostCollectionPut stores (replaces) one member and schedules a debounced
// save — a booting host re-POSTs its whole inventory in a burst.
func hostCollectionPut(kind hostCollectionKind, id string, body map[string]any) {
	host.mu.Lock()
	kind(host)[id] = body
	host.mu.Unlock()
	hostStateSave()
}

// hostCollectionPutPreserving stores (replaces) one member like
// hostCollectionPut, but carries the named keys forward from the existing
// member when the new body omits them - for collections where a periodic
// host report and operator-staged writable properties share the member,
// so the report cannot wipe a value staged between boots.
func hostCollectionPutPreserving(kind hostCollectionKind, id string, body map[string]any, keys ...string) {
	host.mu.Lock()
	if existing, ok := kind(host)[id]; ok {
		for _, k := range keys {
			if _, present := body[k]; !present {
				if v, had := existing[k]; had {
					body[k] = v
				}
			}
		}
	}
	kind(host)[id] = body
	host.mu.Unlock()
	hostStateSave()
}

// hostCollectionMerge applies a shallow PATCH to one member.
func hostCollectionMerge(kind hostCollectionKind, id string, patch map[string]any) map[string]any {
	host.mu.Lock()
	m, ok := kind(host)[id]
	if !ok {
		host.mu.Unlock()
		return nil
	}
	for k, v := range patch {
		m[k] = v
	}
	merged := copyAnyMap(m)
	host.mu.Unlock()
	hostStateSave()
	return merged
}

func hostCollectionDelete(kind hostCollectionKind, id string) bool {
	host.mu.Lock()
	_, ok := kind(host)[id]
	if ok {
		delete(kind(host), id)
	}
	host.mu.Unlock()
	if ok {
		hostStateSave()
	}
	return ok
}

// --- BIOS / SecureBoot state -------------------------------------------------

func hostBiosAttributes() map[string]any {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return copyAnyMap(host.BiosAttributes)
}

// mergeHostBiosAttributes merges a reported attribute set into the live one,
// which is what HTTP PATCH means (DSP0266: a PATCH modifies the properties it
// carries and leaves the rest alone). The host's feature drivers each report
// only the attributes they own, so a wholesale replace let whichever driver
// PATCHed last erase every key the others had published.
//
// A JSON null value deletes its key, per the Redfish convention for clearing
// a property — that is how a host retires an attribute it no longer has, the
// case replace semantics used to cover.
func mergeHostBiosAttributes(attrs map[string]any) {
	host.mu.Lock()
	if host.BiosAttributes == nil {
		host.BiosAttributes = map[string]any{}
	}
	for k, v := range attrs {
		if v == nil {
			delete(host.BiosAttributes, k)
			continue
		}
		host.BiosAttributes[k] = v
	}
	host.mu.Unlock()
	hostStateSave()
}

// setHostBiosAttributes replaces the live attribute set wholesale, which is
// what the v1_1_0 client's writes mean: both its POST (full provision) and
// its PUT (update) carry the complete set — the PUT body is built on top of
// the resource the client just GETd — so replacement also retires attributes
// the firmware no longer has, without needing the explicit nulls the PATCH
// merge relies on.
func setHostBiosAttributes(attrs map[string]any) {
	host.mu.Lock()
	host.BiosAttributes = copyAnyMap(attrs)
	host.mu.Unlock()
	hostStateSave()
}

func hostBiosPending() map[string]any {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return copyAnyMap(host.BiosPending)
}

// setHostBiosPending replaces the staged set and persists immediately: it is
// an operator instruction the host has not read yet. Replacement (not merge)
// is what lets the host clear the stage by PATCHing an empty set after it
// has applied the settings.
func setHostBiosPending(attrs map[string]any) {
	host.mu.Lock()
	host.BiosPending = attrs
	host.mu.Unlock()
	hostStateFlush()
}

func hostBiosRegistry() map[string]any {
	host.mu.RLock()
	defer host.mu.RUnlock()
	if host.BiosRegistry == nil {
		return nil
	}
	return copyAnyMap(host.BiosRegistry)
}

// setHostBiosRegistry stores the published registry and persists immediately.
// The debounce the other host reports use exists to coalesce the burst of
// POSTs a booting host makes; the registry is one document written once per
// firmware change, so there is nothing to coalesce, and a 2s window in which a
// reset drops it is a real way to lose a document nothing republishes until
// the next boot.
func setHostBiosRegistry(reg map[string]any) {
	host.mu.Lock()
	host.BiosRegistry = reg
	host.mu.Unlock()
	hostStateFlush()
}

func hostSecureBoot() map[string]any {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return copyAnyMap(host.SecureBoot)
}

func mergeHostSecureBoot(patch map[string]any) map[string]any {
	host.mu.Lock()
	for k, v := range patch {
		host.SecureBoot[k] = v
	}
	merged := copyAnyMap(host.SecureBoot)
	host.mu.Unlock()
	hostStateSave()
	return merged
}

// --- response plumbing -------------------------------------------------------

// writeHostJSON writes a body with an exact "application/json" content type.
// c.JSON would append "; charset=utf-8", which EDK2's Redfish client rejects.
func writeHostJSON(c *gin.Context, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		redfishErrorResponse(c, http.StatusInternalServerError, "encode response: "+err.Error())
		return
	}
	c.Data(status, "application/json", data)
}

// writeHostResource writes a resource with its ETag (header + @odata.etag)
// and honours If-None-Match with a 304.
func writeHostResource(c *gin.Context, body map[string]any) {
	etag := hostETag(body)
	if etag != "" {
		c.Header("ETag", etag)
		if inm := c.GetHeader("If-None-Match"); inm != "" && hostETagMatches(inm, etag) {
			c.Status(http.StatusNotModified)
			return
		}
		body["@odata.etag"] = etag
	}
	writeHostJSON(c, http.StatusOK, body)
}

// hostView converts a typed resource into the generic map form
// writeHostResource decorates. The marshal round-trip keeps a single wire
// format regardless of whether a handler builds structs or raw maps.
func hostView(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// renderHostMember decorates a stored raw report with the OData preamble the
// URL implies. Id and @odata.id are forced to match the URL so a report that
// carried its own cannot detach the resource from where it is served;
// everything else the host sent is passed through untouched.
func renderHostMember(stored map[string]any, path, id, odataType, ctxFragment, name string) map[string]any {
	m := copyAnyMap(stored)
	m[odataIDKey] = path
	m["Id"] = id
	if _, ok := m[odataTypeKey]; !ok {
		m[odataTypeKey] = odataType
	}
	if _, ok := m["@odata.context"]; !ok {
		m["@odata.context"] = odataContext(ctxFragment)
	}
	if _, ok := m["Name"]; !ok {
		m["Name"] = name
	}
	return m
}

// bindHostBody decodes a host report. Reports are stored raw, so the only
// requirement is that the body is a JSON object.
func bindHostBody(c *gin.Context) (map[string]any, bool) {
	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

// hostMemberID picks the member key for a POSTed report: the first non-empty
// preferred field, else a generated one that does not collide.
func hostMemberID(kind hostCollectionKind, body map[string]any, prefix string, preferred ...string) string {
	for _, field := range preferred {
		if v, ok := body[field].(string); ok && v != "" {
			// Natural keys become URL path segments; keep them one token.
			return strings.ReplaceAll(strings.TrimSpace(v), " ", "-")
		}
	}
	// No natural key in the report. Before minting a fresh id, look for an
	// existing member with identical content: the host re-POSTs its whole
	// inventory every boot, and without this a keyless report accumulates
	// one ghost member per boot (observed on hardware as Memory/DIMM0..42,
	// all copies of the same module).
	if id := hostFindIdentical(kind, body); id != "" {
		return id
	}
	existing := hostCollectionIDs(kind)
	for i := 0; ; i++ {
		id := fmt.Sprintf("%s%d", prefix, i)
		if !slices.Contains(existing, id) {
			return id
		}
	}
}

// hostFindIdentical returns the id of a member whose stored content equals
// the report, or "".
func hostFindIdentical(kind hostCollectionKind, body map[string]any) string {
	host.mu.RLock()
	defer host.mu.RUnlock()
	ids := make([]string, 0, len(kind(host)))
	for id := range kind(host) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if reflect.DeepEqual(kind(host)[id], body) {
			return id
		}
	}
	return ""
}

// --- BootOptions -------------------------------------------------------------
//
// One member per Boot#### variable the host's boot manager enumerates. The
// host re-POSTs the set whenever its BDS re-enumerates; an operator reads
// them to give BootOrder references meaning.

func (s *Service) GetBootOptionCollection(c *gin.Context) {
	ids := hostCollectionIDs(bootOptionsOf)
	links := make([]Link, 0, len(ids))
	for _, id := range ids {
		links = append(links, Link(bootOptionsPath+"/"+id))
	}
	writeHostResource(c, hostView(newCollection(
		"BootOptionCollection", "Boot Option Collection", bootOptionsPath, links...)))
}

func (s *Service) PostBootOption(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	id := hostMemberID(bootOptionsOf, body, "BootOption", "BootOptionReference", "Id")
	hostCollectionPut(bootOptionsOf, id, body)

	path := bootOptionsPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeBootOption, "BootOption.BootOption", id))
}

func (s *Service) GetBootOption(c *gin.Context) {
	id := c.Param("option")
	stored, ok := hostCollectionGet(bootOptionsOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "boot option not found: "+id)
		return
	}
	writeHostResource(c, renderHostMember(stored, bootOptionsPath+"/"+id, id,
		odataTypeBootOption, "BootOption.BootOption", id))
}

func (s *Service) PatchBootOption(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("option")
	current, ok := hostCollectionGet(bootOptionsOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "boot option not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, bootOptionsPath+"/"+id, id,
		odataTypeBootOption, "BootOption.BootOption", id)) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	merged := hostCollectionMerge(bootOptionsOf, id, patch)
	writeHostResource(c, renderHostMember(merged, bootOptionsPath+"/"+id, id,
		odataTypeBootOption, "BootOption.BootOption", id))
}

func (s *Service) DeleteBootOption(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("option")
	current, ok := hostCollectionGet(bootOptionsOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "boot option not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, bootOptionsPath+"/"+id, id,
		odataTypeBootOption, "BootOption.BootOption", id)) {
		return
	}
	hostCollectionDelete(bootOptionsOf, id)
	c.Status(http.StatusNoContent)
}

// --- Memory ------------------------------------------------------------------

func (s *Service) PostMemoryModule(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	id := hostMemberID(memoryOf, body, "DIMM", "Id", "DeviceLocator", "SocketLocator")
	hostCollectionPut(memoryOf, id, body)

	path := memoryPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeMemory, "Memory.Memory", "Memory Module"))
}

func (s *Service) PatchMemoryModule(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("module")
	current, ok := hostCollectionGet(memoryOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "memory module not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, memoryPath+"/"+id, id,
		odataTypeMemory, "Memory.Memory", "Memory Module")) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	merged := hostCollectionMerge(memoryOf, id, patch)
	writeHostResource(c, renderHostMember(merged, memoryPath+"/"+id, id,
		odataTypeMemory, "Memory.Memory", "Memory Module"))
}

// --- Processors --------------------------------------------------------------

func (s *Service) PostProcessor(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	// "CPU" as the fallback stem keeps generated ids in the same shape as the
	// built-in CPU1 the BMC serves before the host has reported anything.
	//
	// The inventory re-POST replaces the member every boot but never carries
	// the operator-writable pair - the firmware's Processor feature driver
	// owns those and PATCHes them separately, later in the same boot. Carry
	// them forward so a value staged between boots survives long enough for
	// the feature driver's consume pass to read it.
	id := hostMemberID(processorsOf, body, "CPU", "Id", "Socket", "ProcessorId")
	hostCollectionPutPreserving(processorsOf, id, body, "SpeedLimitMHz", "SpeedLocked")

	path := processorsPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, odataTypeProcessor, "Processor.Processor", "Processor"))
}

// PatchProcessor merges a write into the stored member - the host updating
// its report, or an operator capping the CPU by PATCHing the standard
// Processor properties (SpeedLimitMHz, SpeedLocked) that the host's
// Processor feature driver consumes and applies on its next boot (the
// direct-resource model the EthernetInterface members use; validation
// belongs to the host, which clamps into what the silicon supports).
// Identity keys are not writable.
func (s *Service) PatchProcessor(c *gin.Context) {
	id := c.Param("processor")
	current, ok := hostCollectionGet(processorsOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "processor not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, processorsPath+"/"+id, id,
		odataTypeProcessor, "Processor.Processor", "Processor")) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	for _, k := range []string{"Id", "@odata.id", "@odata.type", "@odata.context"} {
		delete(patch, k)
	}
	if len(patch) == 0 {
		redfishErrorResponse(c, http.StatusBadRequest, "no writable properties in request")
		return
	}
	merged := hostCollectionMerge(processorsOf, id, patch)
	if merged == nil {
		redfishErrorResponse(c, http.StatusNotFound, "processor not found: "+id)
		return
	}
	writeHostResource(c, renderHostMember(merged, processorsPath+"/"+id, id,
		odataTypeProcessor, "Processor.Processor", "Processor"))
}

func (s *Service) DeleteProcessor(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	id := c.Param("processor")
	if !hostCollectionDelete(processorsOf, id) {
		redfishErrorResponse(c, http.StatusNotFound, "processor not found: "+id)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- Drives (host storage subsystem "1") -------------------------------------

func (s *Service) PostHostDrive(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	if c.Param("storage") != storageID {
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
		return
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	id := hostMemberID(drivesOf, body, "Drive", "Id", "SerialNumber")
	hostCollectionPut(drivesOf, id, body)

	path := drivesPath + "/" + id
	c.Header("Location", path)
	writeHostJSON(c, http.StatusCreated,
		renderHostMember(body, path, id, "#Drive.v1_17_0.Drive", "Drive.Drive", id))
}

func (s *Service) PatchHostDrive(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	if c.Param("storage") != storageID {
		redfishErrorResponse(c, http.StatusNotFound, "storage subsystem not found")
		return
	}
	id := c.Param("drive")
	current, ok := hostCollectionGet(drivesOf, id)
	if !ok {
		redfishErrorResponse(c, http.StatusNotFound, "drive not found: "+id)
		return
	}
	if !hostCheckIfMatch(c, renderHostMember(current, drivesPath+"/"+id, id,
		"#Drive.v1_17_0.Drive", "Drive.Drive", id)) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	merged := hostCollectionMerge(drivesOf, id, patch)
	writeHostResource(c, renderHostMember(merged, drivesPath+"/"+id, id,
		"#Drive.v1_17_0.Drive", "Drive.Drive", id))
}

// --- SecureBoot --------------------------------------------------------------

func secureBootResource() map[string]any {
	m := renderHostMember(hostSecureBoot(), secureBootPath, schemaNameSecureBoot,
		odataTypeSecureBoot, "SecureBoot.SecureBoot", "UEFI Secure Boot")
	return m
}

func (s *Service) GetSecureBoot(c *gin.Context) {
	writeHostResource(c, secureBootResource())
}

// PatchSecureBoot is host-writable: the host's firmware owns the secure-boot
// state and reports transitions (current boot mode, enrolled state) here.
func (s *Service) PatchSecureBoot(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	if !hostCheckIfMatch(c, secureBootResource()) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	mergeHostSecureBoot(patch)
	writeHostResource(c, secureBootResource())
}
