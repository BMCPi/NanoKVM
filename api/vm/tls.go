package vm

import (
	"fmt"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

func (s *Service) SetTls(c *gin.Context) {
	var req proto.SetTlsReq
	var rsp proto.Response

	err := proto.ParseFormRequest(c, &req)
	if err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("invalid arguments: %s", err))
		return
	}

	if req.Enabled {
		err = enableTls()
	} else {
		err = disableTls()
	}

	if err != nil {
		log.Errorf("failed to set TLS: %s", err)
		rsp.ErrRsp(c, -2, "operation failed")
		return
	}

	rsp.OkRsp(c)

	// The proto/cert change only takes effect on the next start; exit and
	// let init respawn us.
	application.RestartService()
}

func enableTls() error {
	if err := utils.GenerateCert(); err != nil {
		return err
	}

	conf, err := config.Read()
	if err != nil {
		return err
	}

	conf.Proto = "https"
	conf.Cert.Crt = "/etc/kvm/server.crt"
	conf.Cert.Key = "/etc/kvm/server.key"

	if err := config.Write(conf); err != nil {
		return err
	}

	return nil
}

func disableTls() error {
	conf, err := config.Read()
	if err != nil {
		return err
	}

	conf.Proto = "http"

	if err := config.Write(conf); err != nil {
		return err
	}

	return nil
}
