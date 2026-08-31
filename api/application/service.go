package application

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

// appUpdateTimeout bounds a self-update: the GitHub release lookup, the
// download and the install. Finite so a stalled download cannot hold the
// global update lock until the next reboot.
const appUpdateTimeout = 30 * time.Minute

// GetVersion reports the running version and the latest release available.
func (h *handlers) GetVersion(c *gin.Context) {
	var rsp proto.Response

	current := application.CurrentVersion()
	h.log.DebugContext(c.Request.Context(), "current version", slog.String("version", current))

	rsp.OkRspWithData(c, &proto.GetVersionRsp{
		Current: current,
		Latest:  application.LatestVersion(c.Request.Context(), h.log),
	})
}

// Update downloads and installs the latest release, then restarts the
// service. RunUpdate owns the global update lock.
func (h *handlers) Update(c *gin.Context) {
	var rsp proto.Response

	// h.d is retained for its process-lifetime context: an application
	// update replaces the running binary and then restarts the process, so
	// it must not be bound to the request that triggered it. See
	// deps.ActionContext.
	ctx, cancel := h.d.ActionContext(appUpdateTimeout)
	defer cancel()

	if err := application.RunUpdate(ctx, h.log); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	h.log.DebugContext(c.Request.Context(), "update application success")
	rsp.OkRsp(c)
	// The process context, not the request's: the restart must outlive this
	// request, and the wait it bounds should only skip at shutdown, not on a
	// browser navigating away.
	application.RestartAfter(h.d.Ctx, application.RestartDelay, h.log)
}
