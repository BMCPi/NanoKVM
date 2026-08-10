package redfish

// bios.go exposes the RPi 5 bootloader EEPROM configuration as the Redfish
// Bios resource (DMTF DSP2046, Bios.v1_2_0). The Attributes map carries
// the bootloader's [all]-section key=value pairs (BOOT_ORDER, BOOT_UART,
// PSU_MAX_CURRENT, etc.); the @Redfish.Settings link points clients at
// the SettingsObject for staging changes.
//
// Staged changes are applied by the host's rpi-eeprom-update on next
// boot — see pkg/firmware/eeprom.go for the on-disk flow.
//
// Endpoints (wired in api/redfish):
//   GET   /redfish/v1/Systems/1/Bios
//   POST  /redfish/v1/Systems/1/Bios/Actions/Bios.ResetBios
//   GET   /redfish/v1/Systems/1/Bios/Settings
//   PATCH /redfish/v1/Systems/1/Bios/Settings
//   GET   /redfish/v1/Systems/1/Bios/AttributeRegistry

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware/eepromkeys"
	"github.com/pi-bmc/nanokvm-app/pkg/smbios"

	"github.com/gin-gonic/gin"
)

// GetBios returns the live bootloader configuration as a Redfish Bios
// resource. Attributes are extracted from the [all] section of the
// bootconf.txt embedded in pieeprom.bin (U-Boot's per-boot EEPROM
// dump).
func (s *Service) GetBios(c *gin.Context) {
	ctrl := firmware.GetController()

	attrs, diag, err := ctrl.GetBIOSAttributes()
	if err != nil {
		redfishErrorResponse(c, http.StatusServiceUnavailable, "read bios attributes: "+err.Error())
		return
	}

	// Prefer the SMBIOS type 0 version the host mirrors into the EEPROM (a
	// clean "2026.04"); the env's ver is the full banner string. Fall back to
	// the env when the tables are absent.
	biosVersion := ""
	if inv, err := ctrl.GetInventory(); err == nil {
		if v, ok := inv["ver"]; ok {
			biosVersion = v
		}
	}
	if info, err := smbios.GetStore().Load(); err == nil && info.BIOSVersion != "" {
		biosVersion = info.BIOSVersion
	}

	// The rpi-eeprom bootloader version/flash-time are exposed as a
	// TrustedComponent SoftwareInventory (trusted_components.go), not here —
	// this resource is U-Boot's BIOS/bootconf surface.
	nanokvmOem := map[string]any{
		odataTypeKey:    "#NanoKVM.v1_0_0.Bios",
		"Diagnostics":   diag,
		"SourceMissing": diag.Source == "",
	}

	c.JSON(http.StatusOK, Bios{
		Resource: Resource{
			ODataType:    "#Bios.v1_2_0.Bios",
			ODataID:      biosPath,
			ODataContext: context("Bios.Bios"),
			ID:           "Bios",
			Name:         "BIOS Configuration",
			Description:  "RPi 5 bootloader EEPROM configuration (bootconf.txt [all] section)",
		},
		AttributeRegistry: "RPiBootloader.1",
		BiosVersion:       biosVersion,
		Attributes:        attrs,
		// @Redfish.Settings — DMTF DSP2046 SettingsObject link. Clients
		// PATCH /Bios/Settings to stage changes; the live Attributes
		// here only update after the host has flashed the EEPROM and
		// rebooted (U-Boot then republishes the live config over I2C).
		Settings: &SettingsAnnotation{
			ODataType:           "#Settings.v1_3_5.Settings",
			SettingsObject:      Link(biosSettingsPath),
			SupportedApplyTimes: []string{"OnReset"},
		},
		Actions: map[string]ActionTarget{
			// We don't currently implement ResetBios (factory defaults
			// would mean re-flashing a fresh upstream image and emptying
			// bootconf.txt). Documented as not allowed.
			"#Bios.ChangePassword": {Target: biosChangePasswordURI},
		},
		Links: Oem{
			"ActiveSoftwareImage": Link(firmwareBIOSPath),
		},
		// Vendor-namespaced OEM block: bootloader provenance (built above)
		// plus diagnostics (DMTF DSP0266 §6.4.13). Diagnostics tell an
		// operator WHY Attributes might be empty: which FAT paths were
		// probed, which parsed, and what section headers they held —
		// useful on first boot before any config is available.
		Oem: Oem{
			"NanoKVM": nanokvmOem,
		},
	})
}

// GetBiosSettings returns the pending bootloader configuration (if any)
// staged in pieeprom.upd. When no update is pending, Attributes is an
// empty object and @Message.ExtendedInfo notes the absence.
func (s *Service) GetBiosSettings(c *gin.Context) {
	ctrl := firmware.GetController()

	attrs, pending, diag, err := ctrl.GetPendingBIOSAttributes()
	if err != nil {
		redfishErrorResponse(c, http.StatusServiceUnavailable, "read pending bios: "+err.Error())
		return
	}
	if attrs == nil {
		attrs = map[string]string{}
	}

	// Distinct human-readable description per state — makes the "empty
	// because nothing is staged" case obvious without having to dig into
	// the Oem diagnostics block. The DMTF Settings spec returns an empty
	// Attributes object when no update is queued; this just adds wording.
	desc := "Bootloader EEPROM changes staged for the next boot (pieeprom.upd). " +
		"PATCH this resource with an Attributes object to stage a change; " +
		"GET /redfish/v1/Systems/1/Bios for the live configuration."
	if !pending {
		desc = "No EEPROM update is staged. " +
			"PATCH this resource with an Attributes object to stage a change; " +
			"GET /redfish/v1/Systems/1/Bios for the live configuration."
	}

	c.JSON(http.StatusOK, Bios{
		Resource: Resource{
			ODataType:    "#Bios.v1_2_0.Bios",
			ODataID:      biosSettingsPath,
			ODataContext: context("Bios.Bios"),
			ID:           "Settings",
			Name:         "BIOS Pending Settings",
			Description:  desc,
		},
		Attributes: attrs,
		Pending:    &pending,
		// Backlink to the live resource — strict Redfish doesn't define
		// this navigation property on the SettingsObject, but it costs
		// nothing and saves operators a doc lookup.
		Links: Oem{
			"LiveBios": Link(biosPath),
		},
		// Vendor-namespaced diagnostics — same pattern as /Bios. When
		// Attributes is empty, this tells the caller WHY: is pieeprom.upd
		// missing entirely, or present-but-malformed, or
		// present-but-only-conditional-sections?
		Oem: Oem{
			"NanoKVM": map[string]any{
				odataTypeKey:  "#NanoKVM.v1_0_0.BiosSettings",
				"Diagnostics": diag,
			},
		},
	})
}

// PatchBiosSettings stages a bootloader EEPROM change. The request body's
// Attributes map replaces the entire [all] section of the staged
// bootconf.txt; conditional sections ([gpio4=1] etc.) currently in the
// source bootconf are preserved verbatim.
//
// Values supplied as JSON numbers or bools are coerced to strings so the
// downstream serializer can write them into the INI-shaped file.
//
// Request body shape:
//
//	{ "Attributes": { "BOOT_ORDER": "0xf41", "BOOT_UART": 1, ... } }
func (s *Service) PatchBiosSettings(c *gin.Context) {
	var req struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if req.Attributes == nil {
		redfishErrorResponse(c, http.StatusBadRequest, "missing Attributes object")
		return
	}

	// Reject unknown keys with a precise error so clients learn which
	// attribute name they got wrong — Redfish 4xx conventions.
	stringAttrs := make(map[string]string, len(req.Attributes))
	for name, raw := range req.Attributes {
		if _, ok := eepromkeys.Lookup(name); !ok {
			redfishErrorResponse(c, http.StatusBadRequest, "unknown attribute "+name)
			return
		}
		stringAttrs[name] = coerceAttribute(raw)
	}

	ctrl := firmware.GetController()
	if _, err := ctrl.SetBIOSAttributes(c.Request.Context(), stringAttrs); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "stage bios update: "+err.Error())
		return
	}

	// Re-read the pending state so the response reflects exactly what was
	// persisted (after default-filtering).
	pending, _, diag, _ := ctrl.GetPendingBIOSAttributes()
	if pending == nil {
		pending = map[string]string{}
	}
	isPending := true
	c.JSON(http.StatusOK, Bios{
		Resource: Resource{
			ODataType:    "#Bios.v1_2_0.Bios",
			ODataID:      biosSettingsPath,
			ODataContext: context("Bios.Bios"),
			ID:           "Settings",
			Name:         "BIOS Pending Settings",
		},
		Attributes: pending,
		Pending:    &isPending,
		Links: Oem{
			"LiveBios": Link(biosPath),
		},
		Oem: Oem{
			"NanoKVM": map[string]any{
				odataTypeKey:  "#NanoKVM.v1_0_0.BiosSettings",
				"Diagnostics": diag,
			},
		},
		ExtendedInfo: []MessageInfo{{
			MessageID: "Base.1.13.SettingsApplyTime",
			Message:   "Settings staged; applied on next system reset.",
			Severity:  "OK",
		}},
	})
}

// GetBiosAttributeRegistry returns the documented attribute schema for
// clients that want to build a structured editor. Backed by the typed
// eepromkeys catalog (raspberrypi/documentation as source of truth).
//
// We return a simplified registry — not a strict #AttributeRegistry
// schema document, but containing the same per-attribute fields a UI
// needs (name, type, default, description, applicability).
func (s *Service) GetBiosAttributeRegistry(c *gin.Context) {
	platform := firmware.PlatformDefault
	entries := eepromkeys.ForPlatform(platform)

	registryEntries := make([]gin.H, 0, len(entries))
	for _, k := range entries {
		registryEntries = append(registryEntries, gin.H{
			"AttributeName": k.Name,
			"Type":          mapBiosAttributeType(k.Type),
			"DefaultValue":  k.Default,
			"HelpText":      k.Description,
			"ReadOnly":      false,
			"ResetRequired": true,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"@odata.type":     "#AttributeRegistry.v1_3_8.AttributeRegistry",
		"@odata.id":       biosRegistryPath,
		"@odata.context":  context("AttributeRegistry.AttributeRegistry"),
		"Id":              "RPiBootloader.1",
		"Name":            "Raspberry Pi Bootloader EEPROM Attribute Registry",
		"Language":        "en",
		"OwningEntity":    "raspberrypi.com",
		"RegistryVersion": "1.0.0",
		"SupportedSystems": []gin.H{{
			"ProductName":              "Raspberry Pi 5",
			"SystemId":                 "1",
			"FirmwareVersion":          "",
			"AttributeRegistryVersion": "1.0.0",
		}},
		"RegistryEntries": gin.H{
			"Attributes": registryEntries,
		},
	})
}

// coerceAttribute renders a JSON value as the string form the bootloader
// expects in bootconf.txt. JSON numbers and bools become their string
// representations; strings pass through.
func coerceAttribute(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "1"
		}
		return "0"
	case float64:
		// JSON numbers always decode as float64. Render integers without
		// a trailing ".0" so BOOT_UART=1 doesn't become BOOT_UART=1.0.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case nil:
		return ""
	default:
		// Fallback — give the bootloader something parseable, even if
		// it's just the Go default print form.
		return fmt.Sprintf("%v", x)
	}
}

// mapBiosAttributeType translates eepromkeys.Type into the closest
// Redfish AttributeRegistry attribute type. Redfish enumerates:
// Enumeration, String, Integer, Boolean, Password.
func mapBiosAttributeType(t eepromkeys.Type) string {
	switch t {
	case eepromkeys.TypeBool:
		return "Boolean"
	case eepromkeys.TypeInt:
		return "Integer"
	case eepromkeys.TypeHex:
		// Hex values are integers expressed in base 16; the wire format
		// is a string so we don't lose the radix prefix.
		return "String"
	default:
		return "String"
	}
}
