package application

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

// appUpdateTimeout bounds a self-update: the GitHub release lookup, the
// download and the install. Finite so a stalled download cannot hold the
// global update lock until the next reboot.
const appUpdateTimeout = 30 * time.Minute

// Service handles application API requests.
type Service struct {
	// Deps is retained for its process-lifetime context. An application
	// update replaces the running binary and then restarts the process, so it
	// must not be bound to the request that triggered it. See
	// deps.ActionContext.
	Deps *deps.Deps
}

// NewService creates a new application service.
func NewService(d *deps.Deps) *Service {
	return &Service{Deps: d}
}

// GetVersion reports the running version and the latest release available.
func (s *Service) GetVersion(c *gin.Context) {
	var rsp proto.Response

	current := application.CurrentVersion()
	log.Debugf("current version: %s", current)

	rsp.OkRspWithData(c, &proto.GetVersionRsp{
		Current: current,
		Latest:  application.LatestVersion(c.Request.Context()),
	})
}

// Update downloads and installs the latest release, then restarts the
// service. RunUpdate owns the global update lock.
func (s *Service) Update(c *gin.Context) {
	var rsp proto.Response

	ctx, cancel := s.Deps.ActionContext(appUpdateTimeout)
	defer cancel()

	if err := application.RunUpdate(ctx); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	log.Debugf("update application success")
	respondAndRestart(c, &rsp)
}

// respondAndRestart acknowledges a successful update, then restarts the
// service after a short delay so the response flushes first.
func respondAndRestart(c *gin.Context, rsp *proto.Response) {
	rsp.OkRsp(c)
	time.Sleep(1 * time.Second)
	application.RestartService()
}
