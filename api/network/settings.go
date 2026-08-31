package network

import (
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/network"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

// GetSettings returns the current network configuration.
func (s *Service) GetSettings(c *gin.Context) {
	var rsp proto.Response
	rsp.OkRspWithData(c, config.GetInstance().Network)
}

// UpdateSettings patches the network configuration (PATCH semantics — only
// fields present in the body change), persists it to /etc/kvm/server.yaml and
// restarts the network manager so the new addressing is applied immediately,
// without a process restart.
func (s *Service) UpdateSettings(c *gin.Context) {
	var rsp proto.Response
	var req struct {
		Enabled *bool `json:"enabled"`
		Eth0    *struct {
			Mode    *string   `json:"mode"`
			MAC     *string   `json:"mac"`
			Address *string   `json:"address"`
			Gateway *string   `json:"gateway"`
			DNS     *[]string `json:"dns"`
		} `json:"eth0"`
		RHI *struct {
			Address *string `json:"address"`
			Lease   *string `json:"lease"`
		} `json:"rhi"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		rsp.ErrRsp(c, -1, "invalid arguments")
		return
	}

	// Patch a copy first so validation failures leave the live config untouched.
	conf := config.GetInstance()
	next := conf.Network

	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.Eth0 != nil {
		if req.Eth0.Mode != nil {
			next.Eth0.Mode = strings.ToLower(*req.Eth0.Mode)
		}
		if req.Eth0.MAC != nil {
			next.Eth0.MAC = *req.Eth0.MAC
		}
		if req.Eth0.Address != nil {
			next.Eth0.Address = *req.Eth0.Address
		}
		if req.Eth0.Gateway != nil {
			next.Eth0.Gateway = *req.Eth0.Gateway
		}
		if req.Eth0.DNS != nil {
			next.Eth0.DNS = *req.Eth0.DNS
		}
	}
	if req.RHI != nil {
		if req.RHI.Address != nil {
			next.RHI.Address = *req.RHI.Address
		}
		if req.RHI.Lease != nil {
			next.RHI.Lease = *req.RHI.Lease
		}
	}

	if err := network.Validate(&next); err != nil {
		rsp.ErrRsp(c, -2, err.Error())
		return
	}

	conf.Network = next
	config.Save()

	// Tear down the running manager and start a fresh one from the updated
	// config so the change takes effect now, not on the next boot.
	network.Restart()

	rsp.OkRspWithData(c, conf.Network)
	slog.InfoContext(c.Request.Context(), "network: settings updated",
		slog.String("eth0Mode", next.Eth0.Mode), slog.Bool("enabled", next.Enabled))
}
