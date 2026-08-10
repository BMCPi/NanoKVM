package network

import (
	"fmt"
	"net"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
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

	if err := validateNetwork(&next); err != nil {
		rsp.ErrRsp(c, -2, err.Error())
		return
	}

	conf.Network = next
	config.Save()

	// Tear down the running manager and start a fresh one from the updated
	// config so the change takes effect now, not on the next boot.
	Restart()

	rsp.OkRspWithData(c, conf.Network)
	log.Infof("network: settings updated (eth0 mode=%s enabled=%t)", next.Eth0.Mode, next.Enabled)
}

// validateNetwork rejects settings the manager could not apply: malformed
// addresses/MACs, an unknown mode, or static mode without an address.
func validateNetwork(n *config.Network) error {
	mode := strings.ToLower(n.Eth0.Mode)
	if mode != "" && mode != ModeDHCP && mode != ModeStatic {
		return fmt.Errorf("invalid eth0 mode %q (want dhcp or static)", n.Eth0.Mode)
	}
	if n.Eth0.MAC != "" {
		if _, err := net.ParseMAC(n.Eth0.MAC); err != nil {
			return fmt.Errorf("invalid eth0 mac %q", n.Eth0.MAC)
		}
	}
	if n.Eth0.Address != "" {
		if _, _, err := net.ParseCIDR(n.Eth0.Address); err != nil {
			return fmt.Errorf("invalid eth0 address %q (want CIDR, e.g. 192.168.1.50/24)", n.Eth0.Address)
		}
	}
	if mode == ModeStatic && n.Eth0.Address == "" {
		return fmt.Errorf("eth0 static mode requires an address")
	}
	if n.Eth0.Gateway != "" && net.ParseIP(n.Eth0.Gateway) == nil {
		return fmt.Errorf("invalid eth0 gateway %q", n.Eth0.Gateway)
	}
	for _, d := range n.Eth0.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("invalid dns server %q", d)
		}
	}
	if n.RHI.Address != "" {
		if _, _, err := net.ParseCIDR(n.RHI.Address); err != nil {
			return fmt.Errorf("invalid rhi address %q (want CIDR, e.g. 169.254.10.1/16)", n.RHI.Address)
		}
	}
	if n.RHI.Lease != "" {
		lease := net.ParseIP(n.RHI.Lease)
		if lease == nil {
			return fmt.Errorf("invalid rhi lease %q", n.RHI.Lease)
		}
		if n.RHI.Address != "" {
			ip, ipnet, _ := net.ParseCIDR(n.RHI.Address)
			if !ipnet.Contains(lease) {
				return fmt.Errorf("rhi lease %s is outside the rhi subnet %s", lease, ipnet)
			}
			if lease.Equal(ip) {
				return fmt.Errorf("rhi lease %s collides with the BMC's own address", lease)
			}
		}
	}
	return nil
}
