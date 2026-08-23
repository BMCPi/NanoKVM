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

// PatchChassisThermal stores the host's thermal report (shallow merge, like
// the other host-owned resources).
func (s *Service) PatchChassisThermal(c *gin.Context) {
	if !hostWritable(c) {
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
