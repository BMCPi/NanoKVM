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

// thermalBody renders Chassis/1/Thermal from the BMC-measured sources: the SoC
// temperature and fan state OP-TEE pushes into this BMC's emulated EEPROM over
// I2C (pkg/bmcsensor), plus any operator-staged fan override this service holds
// for the host's sync driver to poll. The host no longer PATCHes Thermal — it
// reports these over I2C now — so this one function backs both the GET and the
// operator-override If-Match, keeping their ETags in step.
func thermalBody() map[string]any {
	body := map[string]any{}

	if reading, err := socReader.Read(); err == nil {
		// A stale or invalid sample withholds the temperature the same way
		// the SoC sensor does: a die temperature for a host that has gone
		// quiet would be a live-looking lie.
		if !reading.Stale && reading.TempValid() {
			body["Temperatures"] = []any{map[string]any{
				"MemberId":       "SoC",
				"ReadingCelsius": reading.Celsius(),
			}}
		}
		if reading.FanValid() {
			body["Fans"] = []any{map[string]any{
				"MemberId":     "ActiveCooler",
				"Reading":      reading.FanDutyPct,
				"ReadingUnits": "Percent",
				"Oem": map[string]any{"PiBmc": map[string]any{
					"Level":    reading.FanLevel,
					"MaxLevel": reading.FanMaxLevel,
				}},
			}}
		}
	}

	// Merge the operator-staged fan override, kept at top-level
	// Oem.PiBmc.FanOverrideLevel — exactly where RpiRedfishSyncDxe polls it.
	host.mu.RLock()
	stored := copyAnyMap(host.Thermal)
	host.mu.RUnlock()
	if oem, ok := stored["Oem"]; ok {
		body["Oem"] = oem
	}

	return body
}

// GetChassisThermal serves the thermal view the BMC renders from the host's
// I2C sensor push (SoC temperature and fan state), plus any operator-staged
// fan override. Before the first push it is an empty-but-valid resource rather
// than a 404: the host's own driver GETs it, and a 404 ends its walk.
func (s *Service) GetChassisThermal(c *gin.Context) {
	writeHostResource(c, renderHostMember(thermalBody(), chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}

// PatchChassisThermal handles writes to Thermal. Temperature and fan state are
// no longer host-writable — the host reports them over I2C and the BMC renders
// them (see thermalBody) — so only two things can happen here:
//
//   - An operator stages the fan override RpiRedfishSyncDxe polls for:
//     Oem.PiBmc.FanOverrideLevel (integer 0..255 pins the fan; null — or any
//     non-integer — releases it). This is the only operator-writable field.
//   - An older host firmware still PATCHes its thermal report over the host
//     interface. That report is now redundant, so it is accepted and ignored
//     rather than faulted, and the rendered view is returned unchanged.
func (s *Service) PatchChassisThermal(c *gin.Context) {
	if !IsHostInterfaceRequest(c) {
		s.patchThermalFanOverride(c)
		return
	}
	// Accept-and-ignore a legacy host thermal report: the reading now arrives
	// over I2C, so there is nothing to store, but faulting the PATCH would
	// trip an older firmware's sync walk.
	if _, ok := bindHostBody(c); !ok {
		return
	}
	writeHostResource(c, renderHostMember(thermalBody(), chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}

// patchThermalFanOverride is the operator lane of PATCH Chassis/1/Thermal.
// The accepted body is exactly {"Oem":{"PiBmc":{"FanOverrideLevel": n|null}}}
// — an integer 0..255 stages the override the host's sync driver applies
// through RPI_FAN_PROTOCOL, null releases it (the host treats an absent or
// non-integer value as "not steering"). Persisted immediately: it is an
// operator instruction the host has not read yet.
func (s *Service) patchThermalFanOverride(c *gin.Context) {
	if !hostCheckIfMatch(c, renderHostMember(thermalBody(), chassisThermalPath, "Thermal",
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
	host.mu.Unlock()
	hostStateFlush()
	writeHostResource(c, renderHostMember(thermalBody(), chassisThermalPath, "Thermal",
		"#Thermal.v1_7_1.Thermal", "Thermal.Thermal", "Thermal"))
}
