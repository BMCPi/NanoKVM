package redfish

// sensors.go serves the sensors this BMC reads itself, as opposed to the
// reports the managed host publishes.
//
// There is currently one: the host SoC die temperature, which an OP-TEE
// pseudo-TA on the Pi pushes into this BMC's emulated I2C EEPROM from the
// secure world (see pkg/bmcsensor). The same record now also backs
// Chassis/1/Thermal (see thermalBody in chassis.go): once the host stopped
// PATCHing Thermal and began reporting temperature and fan state over I2C,
// Thermal became BMC-rendered too, and the ETag hazard that once kept them
// apart — the host round-tripping Thermal's ETag as If-Match while a
// per-sample value moved it — no longer exists. This resource stays the
// canonical, purpose-built home for the reading; Thermal is the conventional
// thermal-schema view a client expects to find it under.
//
// Sensor is the schema Redfish added for exactly this, so the reading lands
// where a client already looks for BMC-observed telemetry.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/bmcsensor"
	"github.com/stmcginnis/gofish/schemas"
)

// socSensorID is the sensor's Id, and the last segment of its URI.
const socSensorID = "SoCTemp"

// socReader is the process-wide reader. One instance, because staleness is
// measured by watching the sequence number move between reads — a reader
// created per request would think every sample was brand new.
var socReader = bmcsensor.NewReader()

// Sensor is the Redfish Sensor resource (DSP2046 §6.x, Sensor.v1_2_0),
// trimmed to the properties a temperature reading needs.
type Sensor struct {
	Resource
	ReadingType  string `json:"ReadingType"`
	ReadingUnits string `json:"ReadingUnits"`
	// Reading is omitted rather than zeroed when there is nothing
	// trustworthy to report: 0 °C is a plausible temperature, so a client
	// cannot tell it apart from "no reading" if it is always present.
	Reading *float64   `json:"Reading,omitempty"`
	Status  *Status    `json:"Status,omitempty"`
	Oem     *SensorOem `json:"Oem,omitempty"`
}

// SensorOem carries what the pTA's record says beyond the reading itself.
// None of it has a standard Redfish home, and all of it is what an operator
// needs to tell "the host is warm" from "the host stopped talking".
type SensorOem struct {
	NanoKVM SensorOemNanoKVM `json:"NanoKVM"`
}

type SensorOemNanoKVM struct {
	// Sequence is the pTA's sample counter.
	Sequence uint32 `json:"Sequence"`
	// HostUptimeSeconds is seconds since OP-TEE boot at sample time — the
	// host's clock, comparable only to itself.
	HostUptimeSeconds uint32 `json:"HostUptimeSeconds"`
	// Stale reports that the sequence number has stopped advancing, so the
	// reading is the last one the host sent rather than a current one.
	Stale bool `json:"Stale"`
	// TemperatureValid mirrors the pTA's flag for whether the underlying
	// AVS read succeeded.
	TemperatureValid bool `json:"TemperatureValid"`
	// LastPushOK mirrors the pTA's flag for its previous I2C write.
	LastPushOK bool `json:"LastPushOK"`
}

// GetSensorCollection lists the sensors the BMC reads.
//
// The collection is empty rather than absent when the slave EEPROM is not
// configured: a client walking Chassis should find a well-formed collection
// and learn there is nothing in it, not a 404 that ends the walk.
func (s *Service) GetSensorCollection(c *gin.Context) {
	var members []Link
	if socReader.Available() {
		members = append(members, Link(sensorItemPath(socSensorID)))
	}
	c.JSON(http.StatusOK, newCollection(
		"SensorCollection", "Sensor Collection", sensorsPath, members...))
}

// GetSensor serves one sensor.
func (s *Service) GetSensor(c *gin.Context) {
	if c.Param("sensor") != socSensorID {
		redfishErrorResponse(c, http.StatusNotFound, "no such sensor: "+c.Param("sensor"))
		return
	}
	c.JSON(http.StatusOK, socSensorResource())
}

// socSensorResource builds the sensor from whatever the EEPROM currently
// holds.
//
// Every failure to read collapses to the same shape — a sensor that exists
// but is reporting nothing — because from a client's point of view they mean
// the same thing: no trustworthy temperature right now. The distinctions go
// to the log, where they say whether to look at the host, the bus or the
// device tree.
func socSensorResource() Sensor {
	sensor := Sensor{
		Resource: Resource{
			ODataType:    odataTypeSensor,
			ODataID:      sensorItemPath(socSensorID),
			ODataContext: odataContext("Sensor.Sensor"),
			ID:           socSensorID,
			Name:         "Host SoC Temperature",
			Description:  "Managed host die temperature, pushed to this BMC's emulated EEPROM by the host's OP-TEE sensor service",
		},
		ReadingType:  "Temperature",
		ReadingUnits: "Cel",
		Status:       &Status{State: schemas.UnavailableOfflineState},
	}

	reading, err := socReader.Read()
	if err != nil {
		log.Debugf("redfish: SoC sensor unavailable: %v", err)
		return sensor
	}

	sensor.Oem = &SensorOem{NanoKVM: SensorOemNanoKVM{
		Sequence:          reading.Seq,
		HostUptimeSeconds: reading.UptimeSeconds,
		Stale:             reading.Stale,
		TemperatureValid:  reading.TempValid(),
		LastPushOK:        reading.LastPushOK(),
	}}

	// A stale record still parses perfectly — it is simply the last thing a
	// host that has since gone quiet said. Reporting its temperature as a
	// live reading would show a die temperature for a machine that may be
	// powered off, so the value is withheld and the state says why.
	if reading.Stale || !reading.TempValid() {
		return sensor
	}

	celsius := reading.Celsius()
	sensor.Reading = &celsius
	sensor.Status = &Status{State: schemas.EnabledState, Health: schemas.OKHealth}
	return sensor
}
