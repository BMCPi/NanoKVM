package network

import (
	"fmt"
	"net"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// Validate rejects settings the manager could not apply: malformed
// addresses/MACs, an unknown mode, or static mode without an address.
func Validate(n *config.Network) error {
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
