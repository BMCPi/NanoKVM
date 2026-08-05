package redfish

// chassis.go implements the Chassis collection the service root has always
// advertised (it 404'd before). Chassis/1 models the host's baseboard —
// the DSP2046 home for the SMBIOS type-2 values (board manufacturer,
// product, serial) that previously sat under Oem.NanoKVM on the
// ComputerSystem — and carries the standard links binding it to the system
// it contains and the manager that manages it.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/pi-bmc/nanokvm-app/server/service/smbios"
)

// Chassis is the Redfish Chassis resource (DSP2046 §6.13).
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

func (s *Service) GetChassisCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"ChassisCollection", "Chassis Collection", chassisPath,
		Link(chassisItemPath)))
}

func (s *Service) GetChassis(c *gin.Context) {
	res := Chassis{
		Resource: Resource{
			ODataType:    "#Chassis.v1_21_0.Chassis",
			ODataID:      chassisItemPath,
			ODataContext: context("Chassis.Chassis"),
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
	}
	if info, err := smbios.GetStore().Load(); err == nil && info != nil {
		res.Manufacturer = info.BoardManufacturer
		res.Model = info.BoardProduct
		res.SerialNumber = info.BoardSerial
	}
	c.JSON(http.StatusOK, res)
}
