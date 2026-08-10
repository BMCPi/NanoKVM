// Package sysinfo reads the BMC's own identity: OS image version, device
// key, and the addresses of its network interfaces. Shared by the /api/vm
// info handlers and the ui package, which server-renders the same values.
package sysinfo

import (
	"os"
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

// imageVersionMap maps known OS image names (the content of /boot/ver) to
// their released version tags.
var imageVersionMap = map[string]string{
	"2024-06-23-20-59-2d2bfb.img": "v1.0.0",
	"2024-07-23-20-18-587710.img": "v1.1.0",
	"2024-08-08-19-44-bef2ca.img": "v1.2.0",
	"2024-11-13-09-59-9c961a.img": "v1.3.0",
	"2025-02-17-19-08-3649fe.img": "v1.4.0",
	"2025-04-17-14-21-98d17d.img": "v1.4.1",
	"2026-01-05-1_4_1.img":        "v1.4.2",
}

// ImageVersion returns the OS image version tag, the raw image name when the
// image is unknown, or "" when /boot/ver is unreadable.
func ImageVersion() string {
	content, err := os.ReadFile("/boot/ver")
	if err != nil {
		return ""
	}

	image := strings.ReplaceAll(string(content), "\n", "")

	if version, ok := imageVersionMap[image]; ok {
		return version
	}

	return image
}

// DeviceKey returns the device key, or "" when unreadable.
func DeviceKey() string {
	content, err := os.ReadFile("/device_key")
	if err != nil {
		return ""
	}

	return strings.ReplaceAll(string(content), "\n", "")
}

// IPs lists the IPv4 addresses of the up wired/wireless interfaces.
func IPs() (ips []proto.IP) {
	interfaces, err := listInterfaces()
	if err != nil {
		return
	}

	for _, iface := range interfaces {
		if iface.IP.To4() != nil {
			ips = append(ips, proto.IP{
				Name:    iface.Name,
				Addr:    iface.IP.String(),
				Version: "IPv4",
				Type:    iface.Type,
			})
		}
	}

	return
}

// PrimaryIP returns the best display address: the first non-loopback IPv4,
// else the first address of any kind, else "".
func PrimaryIP() string {
	ips := IPs()
	for _, ip := range ips {
		if !strings.HasPrefix(ip.Addr, "127.") {
			return ip.Addr
		}
	}
	if len(ips) > 0 {
		return ips[0].Addr
	}
	return ""
}
