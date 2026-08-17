package redfish

// bios.go serves the Bios resource (DMTF DSP2046, Bios.v1_2_0) from the
// RHI-only host model:
//
//   - GET  /Systems/1/Bios                  the attributes the host reported
//   - PATCH /Systems/1/Bios                 host reports its live attributes
//                                           (host interface only, replace)
//   - GET  /Systems/1/Bios/Settings         the operator-staged pending set
//   - PATCH /Systems/1/Bios/Settings        operator stages attributes for
//                                           the host to apply on next boot
//   - GET  /Systems/1/Bios/AttributeRegistry  the registry the host PUT
//   - PUT  /Systems/1/Bios/AttributeRegistry  host publishes its registry
//
// The BMC validates nothing against a key catalog: the host's firmware owns
// the attribute vocabulary and publishes it via the registry. Attributes are
// stored raw and echoed back; the host reads the pending set over the host
// interface and applies it itself.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// biosResource builds the live Bios document from host state.
func biosResource() Bios {
	reported, _ := HostReported()
	res := Bios{
		Resource: Resource{
			ODataType:    "#Bios.v1_2_0.Bios",
			ODataID:      biosPath,
			ODataContext: context("Bios.Bios"),
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
	if reg := hostBiosRegistry(); reg != nil {
		if id, ok := reg["Id"].(string); ok && id != "" {
			res.AttributeRegistry = id
		} else {
			res.AttributeRegistry = "HostBiosRegistry"
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
			ODataType:    "#Bios.v1_2_0.Bios",
			ODataID:      biosSettingsPath,
			ODataContext: context("Bios.Bios"),
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

// PatchBios is the host's report of its live attributes. Replace, not merge:
// the host reports the complete set it booted with, so a key it no longer
// has must not linger.
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
	setHostBiosAttributes(req.Attributes)
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

// GetBiosAttributeRegistry serves the registry the host published. "Not
// reported yet" is a real state, distinct from an empty registry.
func (s *Service) GetBiosAttributeRegistry(c *gin.Context) {
	reg := hostBiosRegistry()
	if reg == nil {
		redfishErrorResponse(c, http.StatusNotFound,
			"the managed host has not published an attribute registry yet")
		return
	}
	writeHostResource(c, renderHostMember(reg, biosRegistryPath, "BiosAttributeRegistry",
		"#AttributeRegistry.v1_3_8.AttributeRegistry", "AttributeRegistry.AttributeRegistry",
		"BIOS Attribute Registry"))
}

// PutBiosAttributeRegistry stores the registry document the host publishes.
// PUT (not PATCH): the registry is a single document the host replaces
// wholesale when its firmware changes.
func (s *Service) PutBiosAttributeRegistry(c *gin.Context) {
	if !hostWritable(c) {
		return
	}
	if current := hostBiosRegistry(); current != nil {
		if !hostCheckIfMatch(c, renderHostMember(current, biosRegistryPath, "BiosAttributeRegistry",
			"#AttributeRegistry.v1_3_8.AttributeRegistry", "AttributeRegistry.AttributeRegistry",
			"BIOS Attribute Registry")) {
			return
		}
	}
	body, ok := bindHostBody(c)
	if !ok {
		return
	}
	setHostBiosRegistry(body)
	writeHostResource(c, renderHostMember(body, biosRegistryPath, "BiosAttributeRegistry",
		"#AttributeRegistry.v1_3_8.AttributeRegistry", "AttributeRegistry.AttributeRegistry",
		"BIOS Attribute Registry"))
}
