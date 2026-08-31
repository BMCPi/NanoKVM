package vm

import (
	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	"github.com/pi-bmc/nanokvm-app/pkg/protocol/discovery"
	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
)

func (h *handlers) GetInfo(c *gin.Context) {
	var rsp proto.Response

	data := &proto.GetInfoRsp{
		IPs:         sysinfo.IPs(h.log),
		Mdns:        getMdns(),
		Image:       sysinfo.ImageVersion(),
		Application: application.CurrentVersion(),
		DeviceKey:   sysinfo.DeviceKey(),
	}

	rsp.OkRspWithData(c, data)
	h.log.DebugContext(c.Request.Context(), "get vm information success")
}

func getMdns() string {
	// Report the built-in mDNS responder's advertised name (e.g.
	// "licheervnano.local"), or "" when the responder is disabled/not running.
	// This replaced the old avahi-daemon PID-file probe.
	name, ok := discovery.Advertised()
	if !ok {
		return ""
	}
	return name
}

func (h *handlers) GetHardware(c *gin.Context) {
	var rsp proto.Response

	conf := config.GetInstance()
	version := conf.Hardware.Version.String()

	rsp.OkRspWithData(c, &proto.GetHardwareRsp{
		Version: version,
	})
}
