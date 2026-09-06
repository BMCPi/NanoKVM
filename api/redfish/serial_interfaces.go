package redfish

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"

	"github.com/gin-gonic/gin"
	"github.com/stmcginnis/gofish/schemas"
)

// numOrString accepts a JSON number or a JSON string holding a number.
//
// Redfish spells BitRate/DataBits/StopBits as string enums ("115200", "8"),
// so a conformant client PATCHes strings. This service used to serve and
// accept plain numbers, and the local UI still sends them — so we take
// either and normalise on the way out.
type numOrString struct {
	Value int
	Set   bool
}

func (n *numOrString) UnmarshalJSON(b []byte) error {
	var i int
	if err := json.Unmarshal(b, &i); err == nil {
		n.Value, n.Set = i, true
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("expected a number or numeric string, got %q", s)
	}
	n.Value, n.Set = i, true
	return nil
}

type serialPatchRequest struct {
	BitRate     numOrString `json:"BitRate"`
	Parity      string      `json:"Parity"`
	DataBits    numOrString `json:"DataBits"`
	StopBits    numOrString `json:"StopBits"`
	FlowControl string      `json:"FlowControl"`
}

func (h *handlers) GetSerialInterfaceCollection(c *gin.Context) {
	c.JSON(http.StatusOK, newCollection(
		"SerialInterfaceCollection", "Serial Interface Collection", serialInterfacesPath,
		Link(serialInterfacePath),
	))
}

func (h *handlers) GetSerialInterface(c *gin.Context) {
	c.JSON(http.StatusOK, buildSerialInterfaceResource())
}

// saveConfig is config.Save behind a variable so tests can observe that a
// Redfish write actually reached the config store. Never reassigned outside
// tests.
var saveConfig = config.Save

func (h *handlers) PatchSerialInterface(c *gin.Context) {
	var req serialPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		redfishErrorResponse(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	cfg := config.GetInstance()

	if req.BitRate.Set {
		cfg.Serial.BaudRate = req.BitRate.Value
	}
	if req.Parity != "" {
		cfg.Serial.Parity = strings.ToLower(req.Parity)
	}
	if req.DataBits.Set {
		cfg.Serial.DataBits = req.DataBits.Value
	}
	if req.StopBits.Set {
		cfg.Serial.StopBits = req.StopBits.Value
	}
	if req.FlowControl != "" {
		cfg.Serial.FlowControl = strings.ToLower(req.FlowControl)
	}

	// Persist: this handler edits the shared config instance in place, and
	// without a save the new settings live only until the process restarts.
	// Every other settings surface in the app saves; Redfish did not.
	saveConfig()

	// NOTE: active serial broker sessions will not pick up new settings
	// until the next Connect(). A broker restart may be needed.
	h.log.DebugContext(c.Request.Context(), "redfish serial interface updated via central config")
	c.JSON(http.StatusOK, buildSerialInterfaceResource())
}

// titleCase capitalises the first letter of a lower-case config value
// so the Redfish response uses the conventional "None" / "Even" / "Odd" form.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// itoaOrEmpty renders a positive int; zero (unset config) yields "" so the
// property is omitted rather than serialised as an invalid empty enum.
func itoaOrEmpty(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

func buildSerialInterfaceResource() SerialInterface {
	cfg := config.GetInstance().Serial
	broker := serial.GetBroker()

	return SerialInterface{
		Resource: Resource{
			ODataType:    "#SerialInterface.v1_1_7.SerialInterface",
			ODataID:      serialInterfacePath,
			ODataContext: odataContext("SerialInterface.SerialInterface"),
			ID:           "1",
			Name:         "Serial Interface 1",
		},
		InterfaceEnabled: broker.Active(),
		// These three are string enums in the schema, not numbers — see the
		// SerialInterface type in resources.go.
		BitRate:     schemas.BitRate(itoaOrEmpty(cfg.BaudRate)),
		DataBits:    schemas.DataBits(itoaOrEmpty(cfg.DataBits)),
		StopBits:    schemas.StopBits(itoaOrEmpty(cfg.StopBits)),
		Parity:      schemas.Parity(titleCase(cfg.Parity)),
		FlowControl: schemas.SerialInferfaceFlowControl(titleCase(cfg.FlowControl)),
	}
}
