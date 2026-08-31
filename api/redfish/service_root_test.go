package redfish

import (
	"log/slog"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

// ServiceRoot and Manager must agree: a client that keys off either one has
// to land on the same device.
func TestServiceRootAndManagerShareUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	svc := NewService(&deps.Deps{
		Power:    power.NewController(config.Hardware{}, config.Power{}, slog.New(slog.DiscardHandler)),
		Firmware: firmware.NewController(cfg, slog.New(slog.DiscardHandler)),
	})
	r := gin.New()
	r.GET(ServiceRootPath, svc.GetServiceRoot)
	r.GET(managerPath, svc.GetManager)

	root := mustGetJSON(t, r, ServiceRootPath)
	mgr := mustGetJSON(t, r, managerPath)

	if managerUUID() == "" {
		t.Skip("no stable identity source on this host")
	}
	if root["UUID"] != mgr["UUID"] {
		t.Errorf("ServiceRoot UUID %v != Manager UUID %v", root["UUID"], mgr["UUID"])
	}
}
