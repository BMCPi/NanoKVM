package vm

import (
	"github.com/pi-bmc/nanokvm-app/pkg/config"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/mdns"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
)

func (s *Service) GetInfo(c *gin.Context) {
	var rsp proto.Response

	data := &proto.GetInfoRsp{
		IPs:         sysinfo.IPs(),
		Mdns:        getMdns(),
		Image:       sysinfo.ImageVersion(),
		Application: application.CurrentVersion(),
		DeviceKey:   sysinfo.DeviceKey(),
	}

	rsp.OkRspWithData(c, data)
	log.Debug("get vm information success")
}

func getMdns() string {
	// Report the built-in mDNS responder's advertised name (e.g.
	// "licheervnano.local"), or "" when the responder is disabled/not running.
	// This replaced the old avahi-daemon PID-file probe.
	name, ok := mdns.Advertised()
	if !ok {
		return ""
	}
	return name
}

func (s *Service) GetHardware(c *gin.Context) {
	var rsp proto.Response

	conf := config.GetInstance()
	version := conf.Hardware.Version.String()

	rsp.OkRspWithData(c, &proto.GetHardwareRsp{
		Version: version,
	})
}
