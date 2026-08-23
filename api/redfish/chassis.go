package redfish

// chassis.go implements the Chassis collection the service root advertises.
// Chassis/1 models the host's baseboard and carries the standard links
// binding it to the system it contains and the manager that manages it.
// Board identity (manufacturer, product, serial) used to come from the
// EEPROM's SMBIOS mirror; without a host report for it the properties are
// honestly omitted.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// Chassis is the Redfish Chassis resource (DSP2046 §6.13).
// ChassisThermal is emitted as a link so the host's thermal feature driver
// can find its branch; the resource itself is host-reported.
type Chassis struct {
	Resource
	ChassisType  schemas.ChassisType `json:"ChassisType"`
	Manufacturer string              `json:"Manufacturer,omitempty"`
	Model        string              `json:"Model,omitempty"`
	SerialNumber string              `json:"SerialNumber,omitempty"`
	Status       *Status             `json:"Status,omitempty"`
	Links        ChassisLinks        `json:"Links"`
}

type ChassisLinks struct {
	ComputerSystems Links `json:"ComputerSystems"`
	ManagedBy       Links `json:"ManagedBy"`
}

// chassisWithThermal decorates the Chassis JSON with the Thermal link. The
// struct route would force Thermal into every Links consumer; a top-level
// property is what the schema wants anyway.
type chassisWithThermal struct {
	Chassis
	Thermal Link `json:"Thermal"`
	// Sensors is the BMC's own readings, kept separate from Thermal so
	// host-reported and BMC-measured data stay distinguishable.
	Sensors Link `json:"Sensors"`
}

func (s *Service) GetChassisCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ChassisCollection", "Chassis Collection", chassisPath,
		Link(chassisItemPath)))
}

func (s *Service) GetChassis(c *gin.Context) {
	c.JSON(http.StatusOK, chassisWithThermal{
		Thermal: Link(chassisThermalPath),
		Sensors: Link(sensorsPath),
		Chassis: Chassis{
			Resource: Resource{
				ODataType:    "#Chassis.v1_21_0.Chassis",
				ODataID:      chassisItemPath,
				ODataContext: odataContext("Chassis.Chassis"),
				ID:           "1",
				Name:         "Host Baseboard",
			},
			// The baseboard of the managed host; "Module" is the conventional
			// ChassisType for a board exposed separately from its enclosure.
			ChassisType: schemas.ModuleChassisType,
			Status:      &Status{State: schemas.EnabledState, Health: schemas.OKHealth},
			Links: ChassisLinks{
				ComputerSystems: Links{Link(systemPath)},
				ManagedBy:       Links{Link(managerPath)},
			},
		},
	})
}

// GetChassisThermal serves the thermal report the host published. Before the
// first report it answers with an empty-but-valid resource rather than 404:
// the host's own driver GETs before it PATCHes, and a 404 ends its walk.
func (s *Service) GetChassisThermal(c *gin.Context) {
	host.mu.RLock()
	stored := copyAnyMap(host.Thermal)
	host.mu.RUnlock()
	writeHostResource(c, renderHostMember(stored, chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}

// PatchChassisThermal carries both write directions on Thermal, split by
// origin the way PATCH /Systems/1 is:
//
//   - The host's firmware reports temperatures and fan state (shallow
//     top-level merge, like the other host-owned resources). Its Oem data is
//     nested inside Fans[0], so the merge never touches the top-level Oem
//     block the operator stages into.
//   - An operator stages the fan override RpiRedfishSyncDxe polls for:
//     Oem.PiBmc.FanOverrideLevel (integer 0..255 pins the fan; null — or any
//     non-integer — releases it). Nothing else on the resource is
//     operator-writable, because everything else is a host report.
func (s *Service) PatchChassisThermal(c *gin.Context) {
	if !IsHostInterfaceRequest(c) {
		s.patchThermalFanOverride(c)
		return
	}
	host.mu.RLock()
	current := copyAnyMap(host.Thermal)
	host.mu.RUnlock()
	if !hostCheckIfMatch(c, renderHostMember(current, chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal")) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	host.mu.Lock()
	if host.Thermal == nil {
		host.Thermal = map[string]any{}
	}
	for k, v := range patch {
		host.Thermal[k] = v
	}
	merged := copyAnyMap(host.Thermal)
	host.mu.Unlock()
	hostStateSave()
	writeHostResource(c, renderHostMember(merged, chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}

// patchThermalFanOverride is the operator lane of PATCH Chassis/1/Thermal.
// The accepted body is exactly {"Oem":{"PiBmc":{"FanOverrideLevel": n|null}}}
// — an integer 0..255 stages the override the host's sync driver applies
// through RPI_FAN_PROTOCOL, null releases it (the host treats an absent or
// non-integer value as "not steering"). Persisted immediately: it is an
// operator instruction the host has not read yet.
func (s *Service) patchThermalFanOverride(c *gin.Context) {
	host.mu.RLock()
	current := copyAnyMap(host.Thermal)
	host.mu.RUnlock()
	if !hostCheckIfMatch(c, renderHostMember(current, chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal")) {
		return
	}
	patch, ok := bindHostBody(c)
	if !ok {
		return
	}
	reject := func() {
		redfishErrorResponse(c, http.StatusForbidden,
			"Thermal is reported by the managed host; operators may only stage Oem.PiBmc.FanOverrideLevel")
	}
	if len(patch) != 1 {
		reject()
		return
	}
	oemPatch, ok := patch["Oem"].(map[string]any)
	if !ok || len(oemPatch) != 1 {
		reject()
		return
	}
	piBmcPatch, ok := oemPatch["PiBmc"].(map[string]any)
	if !ok || len(piBmcPatch) != 1 {
		reject()
		return
	}
	raw, ok := piBmcPatch["FanOverrideLevel"]
	if !ok {
		reject()
		return
	}
	// null releases the override; an integer 0..255 stages it.
	var level *int64
	if raw != nil {
		f, isNum := raw.(float64)
		if !isNum || f != float64(int64(f)) || f < 0 || f > 255 {
			redfishErrorResponse(c, http.StatusBadRequest,
				"FanOverrideLevel must be an integer 0..255, or null to release")
			return
		}
		n := int64(f)
		level = &n
	}

	host.mu.Lock()
	if host.Thermal == nil {
		host.Thermal = map[string]any{}
	}
	oem, _ := host.Thermal["Oem"].(map[string]any)
	if oem == nil {
		oem = map[string]any{}
	}
	piBmc, _ := oem["PiBmc"].(map[string]any)
	if piBmc == nil {
		piBmc = map[string]any{}
	}
	if level == nil {
		delete(piBmc, "FanOverrideLevel")
	} else {
		piBmc["FanOverrideLevel"] = *level
	}
	oem["PiBmc"] = piBmc
	host.Thermal["Oem"] = oem
	merged := copyAnyMap(host.Thermal)
	host.mu.Unlock()
	hostStateFlush()
	writeHostResource(c, renderHostMember(merged, chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}
