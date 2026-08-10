package application

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

// Service handles application API requests.
type Service struct{}

// NewService creates a new application service.
func NewService() *Service {
	return &Service{}
}

// GetVersion reports the running version and the latest release available.
func (s *Service) GetVersion(c *gin.Context) {
	var rsp proto.Response

	current := application.CurrentVersion()
	log.Debugf("current version: %s", current)

	rsp.OkRspWithData(c, &proto.GetVersionRsp{
		Current: current,
		Latest:  application.LatestVersion(),
	})
}

// Update downloads and installs the latest release, then restarts the
// service. RunUpdate owns the global update lock.
func (s *Service) Update(c *gin.Context) {
	var rsp proto.Response

	if err := application.RunUpdate(); err != nil {
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
