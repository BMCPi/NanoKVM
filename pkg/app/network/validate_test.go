package network

import (
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

func TestValidate(t *testing.T) {
	base := func() config.Network {
		return config.Network{
			Enabled: true,
			Eth0:    config.InterfaceConfig{Name: "eth0", Mode: "dhcp"},
			RHI:     config.RHIConfig{Interface: "usb0", Address: "169.254.10.1/16"},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*config.Network)
		wantErr bool
	}{
		{"defaults ok", func(*config.Network) {}, false},
		{"static with address", func(n *config.Network) {
			n.Eth0.Mode = "static"
			n.Eth0.Address = "192.168.1.50/24"
			n.Eth0.Gateway = "192.168.1.1"
			n.Eth0.DNS = []string{"8.8.8.8", "1.1.1.1"}
		}, false},
		{"static without address", func(n *config.Network) {
			n.Eth0.Mode = "static"
		}, true},
		{"bad mode", func(n *config.Network) { n.Eth0.Mode = "bridge" }, true},
		{"bad mac", func(n *config.Network) { n.Eth0.MAC = "not-a-mac" }, true},
		{"good mac", func(n *config.Network) { n.Eth0.MAC = "aa:bb:cc:dd:ee:ff" }, false},
		{"address not cidr", func(n *config.Network) {
			n.Eth0.Mode = "static"
			n.Eth0.Address = "192.168.1.50"
		}, true},
		{"bad gateway", func(n *config.Network) { n.Eth0.Gateway = "nope" }, true},
		{"bad dns entry", func(n *config.Network) { n.Eth0.DNS = []string{"8.8.8.8", "x"} }, true},
		{"bad rhi address", func(n *config.Network) { n.RHI.Address = "169.254.10.1" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := base()
			tc.mutate(&n)
			err := Validate(&n)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateNetwork = %v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}
