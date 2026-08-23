package redfish

// bios.go serves the Bios resource (DMTF DSP2046, Bios.v1_0_9) from the
// RHI-only host model. The version is pinned to the one the managed host's
// Redfish client is built against
// (RedfishClientPkg/Features/Bios/v1_0_9), so the documents this BMC serves
// parse on the client's normal path rather than its compatibility fallback:
//
//   - GET  /Systems/1/Bios                  the attributes the host reported
//   - PATCH /Systems/1/Bios                 host reports its live attributes
//                                           (host interface only, merge)
//   - GET  /Systems/1/Bios/Settings         the operator-staged pending set
//   - PATCH /Systems/1/Bios/Settings        operator stages attributes for
//                                           the host to apply on next boot
//   - GET  /Systems/1/Bios/AttributeRegistry  the registry the host PUT
//   - PUT  /Systems/1/Bios/AttributeRegistry  publishes the registry (host
//                                             interface, or an authenticated
//                                             operator over the LAN)
//
// The BMC validates nothing against a key catalog: the host's firmware owns
// the attribute vocabulary and publishes it via the registry. Attributes are
// stored raw and echoed back; the host reads the pending set over the host
// interface and applies it itself.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// biosResource builds the live Bios document from host state.
func biosResource() Bios {
	reported, _ := HostReported()
	res := Bios{
		Resource: Resource{
			ODataType:    odataTypeBios,
			ODataID:      biosPath,
			ODataContext: odataContext("Bios.Bios"),
			ID:           "Bios",
			Name:         "BIOS Configuration",
			Description:  "Firmware attributes as last reported by the managed host",
		},
		BiosVersion: reported.BiosVersion,
		Attributes:  hostBiosAttributes(),
		// @Redfish.Settings — DMTF settings-object pattern. Operators PATCH
		// the SettingsObject; the host firmware reads it over the host
		// interface and applies it on its next boot, then re-reports the
		// live Attributes here.
		Settings: &SettingsAnnotation{
			ODataType:           "#Settings.v1_3_5.Settings",
			SettingsObject:      Link(biosSettingsPath),
			SupportedApplyTimes: []string{"OnReset"},
		},
	}
	// Always advertised, even before the host publishes: the EDK2 client
	// derives the registry's URI from this property, so an absent value
	// leaves it with nowhere to PUT the document in the first place.
	res.AttributeRegistry = biosRegistryName
	if reg := hostBiosRegistry(); reg != nil {
		if id, ok := reg["Id"].(string); ok && id != "" {
			res.AttributeRegistry = id
		}
	}
	return res
}

// biosSettingsResource builds the pending-settings document.
func biosSettingsResource() Bios {
	pending := hostBiosPending()
	desc := "Attributes staged for the host firmware to apply on its next boot. " +
		"PATCH this resource with an Attributes object to stage a change; " +
		"the submitted set replaces the staged one."
	if len(pending) == 0 {
		desc = "No attributes are staged. " +
			"PATCH this resource with an Attributes object to stage a change."
	}
	return Bios{
		Resource: Resource{
			ODataType:    odataTypeBios,
			ODataID:      biosSettingsPath,
			ODataContext: odataContext("Bios.Bios"),
			ID:           "Settings",
			Name:         "BIOS Pending Settings",
			Description:  desc,
		},
		Attributes: pending,
	}
}

// GetBios returns the attributes the host last reported.
func (s *Service) GetBios(c *gin.Context) {
	writeHostResource(c, hostView(biosResource()))
}

// PatchBios is the host's report of its live attributes. Merge, not replace:
// the reporting host is several independent feature drivers, each PATCHing
// only the attributes it owns, so replacing the set would let whichever one
// reported last wipe every key the others published. Omitted keys keep their
// stored value; an explicit null deletes one (see mergeHostBiosAttributes).
func (s *Service) PatchBios(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	if !hostCheckIfMatch(c, hostView(biosResource())) {
		return
	}
	var req struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Attributes == nil {
		redfishErrorResponse(c, http.StatusBadRequest, "missing Attributes object")
		return
	}
	mergeHostBiosAttributes(req.Attributes)
	writeHostResource(c, hostView(biosResource()))
}

// GetBiosSettings returns the operator-staged pending attributes.
func (s *Service) GetBiosSettings(c *gin.Context) {
	writeHostResource(c, hostView(biosSettingsResource()))
}

// PatchBiosSettings stages attributes for the host's next boot. Normal
// (operator) authentication — this is the one Bios write that is not
// host-owned. The submitted Attributes replace the staged set, which also
// lets the host clear the stage (PATCH {"Attributes": {}}) after applying.
func (s *Service) PatchBiosSettings(c *gin.Context) {
	if !hostCheckIfMatch(c, hostView(biosSettingsResource())) {
		return
	}
	var req struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Attributes == nil {
		redfishErrorResponse(c, http.StatusBadRequest, "missing Attributes object")
		return
	}
	setHostBiosPending(req.Attributes)

	res := biosSettingsResource()
	res.ExtendedInfo = []MessageInfo{{
		MessageID: "Base.1.13.SettingsApplyTime",
		Message:   "Settings staged; the host firmware applies them on its next boot.",
		Severity:  "OK",
	}}
	writeHostResource(c, hostView(res))
}

// biosRegistryName is the default AttributeRegistry name. EDK2's client
// derives the registry URI as <parent of Bios>/<this value>, and names its
// own generated documents BiosAttributeRegistry[.vX_Y_Z].
const biosRegistryName = "BiosAttributeRegistry"

// biosRegistryNameOK accepts the names a client legitimately resolves the
// registry under: the advertised default (with or without a version suffix)
// and the pre-pivot "AttributeRegistry" spelling.
func biosRegistryNameOK(name string) bool {
	return name == "AttributeRegistry" || strings.HasPrefix(name, biosRegistryName)
}

// registryResource renders the stored registry at the URI it was asked for —
// the client treats the path it derived as canonical, so the @odata.id must
// agree with it.
func registryResource(reg map[string]any, name string) map[string]any {
	return renderHostMember(reg, biosPath+"/"+name, name,
		odataTypeAttributeRegistry, "AttributeRegistry.AttributeRegistry",
		"BIOS Attribute Registry")
}

// baseBiosRegistry is the skeleton served before the host publishes. Per the
// RedfishClientPkg contract the BMC provides the *base* registry resource —
// the feature driver GETs it before deciding to PUT its generated document,
// and a 404 here ends its walk instead of triggering provisioning.
func baseBiosRegistry() map[string]any {
	return map[string]any{
		"Language":        "en",
		"RegistryVersion": "1.0.0",
		"OwningEntity":    "NanoKVM",
		"RegistryEntries": map[string]any{"Attributes": []any{}},
	}
}

// GetBiosAttributeRegistry serves the registry the host published, or the
// base skeleton before it has.
func (s *Service) GetBiosAttributeRegistry(c *gin.Context) {
	name := c.Param("registry")
	if !biosRegistryNameOK(name) {
		redfishErrorResponse(c, http.StatusNotFound, "no such Bios sub-resource")
		return
	}
	reg := hostBiosRegistry()
	if reg == nil {
		reg = baseBiosRegistry()
	}
	writeHostResource(c, registryResource(reg, name))
}

// PutBiosAttributeRegistry stores the registry document. PUT (not PATCH): the
// registry is a single document that is replaced wholesale when the firmware's
// attribute vocabulary changes.
//
// Unlike the other host-owned resources this one is NOT gated on hostWritable.
// That gate exists because a report ("this machine has 64 GB of RAM") is a
// claim of fact the BMC would otherwise repeat back as truth, and only the host
// can make it. The registry is not a claim about hardware — it is the schema
// describing which attribute keys exist and what values they accept. Letting an
// operator seed it is useful in its own right: EDK2's BiosAttributeRegistryDxe
// GETs this resource before it decides to publish, so a registry loaded out of
// band gives clients a vocabulary to work against on a host that has not booted
// far enough to publish its own. CheckAuth still applies, so a LAN caller needs
// operator credentials, while the host interface stays unauthenticated per
// DSP0270.
func (s *Service) PutBiosAttributeRegistry(c *gin.Context) {
	name := c.Param("registry")
	if !biosRegistryNameOK(name) {
		redfishErrorResponse(c, http.StatusNotFound, "no such Bios sub-resource")
		return
	}
	if current := hostBiosRegistry(); current != nil {
		if !hostCheckIfMatch(c, registryResource(current, name)) {
			return
		}
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	setHostBiosRegistry(body)
	writeHostResource(c, registryResource(body, name))
}

// GetRegistries serves the registry-file collection BiosAttributeRegistryDxe
// walks from the service root.
func (s *Service) GetRegistries(c *gin.Context) {
	writeHostResource(c, map[string]any{
		"@odata.type":         "#MessageRegistryFileCollection.MessageRegistryFileCollection",
		"@odata.id":           registriesPath,
		"Name":                "Registry File Collection",
		"Members@odata.count": 1,
		"Members":             []map[string]any{{"@odata.id": registryFilePath}},
	})
}

// GetRegistryFile points at where the registry document actually lives: the
// host PUTs it under the Bios resource, so a /Registries-relative URI here
// would name something nothing serves.
func (s *Service) GetRegistryFile(c *gin.Context) {
	if c.Param("id") != biosRegistryName {
		redfishErrorResponse(c, http.StatusNotFound, "registry not found")
		return
	}
	writeHostResource(c, map[string]any{
		"@odata.type": "#MessageRegistryFile.v1_1_0.MessageRegistryFile",
		"@odata.id":   registryFilePath,
		"Id":          biosRegistryName,
		"Name":        "BIOS Attribute Registry",
		"Registry":    biosRegistryName,
		"Languages":   []string{"en"},
		"Location": []map[string]any{
			{"Language": "en", "Uri": biosRegistryPath},
		},
	})
}
