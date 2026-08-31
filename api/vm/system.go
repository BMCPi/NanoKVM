package vm

import (
	"context"
	"os/exec"

	"github.com/pi-bmc/nanokvm-app/pkg/proto"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func (s *Service) Reboot(c *gin.Context) {
	var rsp proto.Response

	log.Println("reboot system...")

	// context.Background(), not c.Request.Context(): a client disconnect must
	// not abort a reboot partway through, so the command's lifetime is
	// intentionally not tied to the request.
	err := exec.CommandContext(context.Background(), "reboot").Run()
	if err != nil {
		rsp.ErrRsp(c, -1, "operation failed")
		log.Errorf("failed to reboot: %s", err)
		return
	}

	rsp.OkRsp(c)
	log.Debug("system rebooted")
}
