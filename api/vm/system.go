package vm

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/pi-bmc/nanokvm-app/pkg/proto"

	"github.com/gin-gonic/gin"
)

func (s *Service) Reboot(c *gin.Context) {
	var rsp proto.Response

	slog.InfoContext(c.Request.Context(), "reboot system")

	// context.Background(), not c.Request.Context(): a client disconnect must
	// not abort a reboot partway through, so the command's lifetime is
	// intentionally not tied to the request. This repo's idiom for exactly
	// this problem is deps.ActionContext (see api/vm/service.go's Deps field
	// doc and api/vm/gpio.go's power handlers) -- deliberately not adopted
	// here because it would add timeout and shutdown-cancellation semantics
	// this command does not have today, which is a behaviour change out of
	// scope for a lint-only pass.
	err := exec.CommandContext(context.Background(), "reboot").Run()
	if err != nil {
		rsp.ErrRsp(c, -1, "operation failed")
		slog.ErrorContext(c.Request.Context(), "failed to reboot", slog.Any("err", err))
		return
	}

	rsp.OkRsp(c)
	slog.DebugContext(c.Request.Context(), "system rebooted")
}
