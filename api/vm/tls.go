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

func (s *Service) SetTLS(c *gin.Context) {
	var req proto.SetTLSReq
	var rsp proto.Response

	err := proto.ParseFormRequest(c, &req)
	if err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("invalid arguments: %s", err))
		return
	}

	if req.Enabled {
		err = EnableTLS()
	} else {
		err = DisableTLS()
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

// EnableTLS generates a self-signed certificate and switches the persisted
// proto to https. Exported so the UI's settings fragment applies TLS through
// the same path as the JSON API rather than duplicating the config writes.
func EnableTLS() error {
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

	return config.Write(conf)
}

// DisableTLS switches the persisted proto back to http.
func DisableTLS() error {
	conf, err := config.Read()
	if err != nil {
		return err
	}

	conf.Proto = "http"

	return config.Write(conf)
}
