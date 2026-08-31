package vm

import (
	"fmt"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/middleware"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

func (h *handlers) SetTLS(c *gin.Context) {
	var req proto.SetTLSReq
	var rsp proto.Response

	err := proto.ParseFormRequest(c, h.log, &req)
	if err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("invalid arguments: %s", err))
		return
	}

	if req.Enabled {
		err = EnableTLS(h.log)
	} else {
		err = DisableTLS()
	}

	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to set TLS", slog.Any("err", err))
		rsp.ErrRsp(c, -2, "operation failed")
		return
	}

	rsp.OkRsp(c)

	// The proto/cert change only takes effect on the next start; exit and
	// let init respawn us.
	application.RestartService(h.log)
}

// EnableTLS generates a self-signed certificate and switches the persisted
// proto to https. Exported so the UI's settings fragment applies TLS through
// the same path as the JSON API rather than duplicating the config writes.
func EnableTLS(log *slog.Logger) error {
	if err := middleware.GenerateCert(log); err != nil {
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
