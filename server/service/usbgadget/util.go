package usbgadget

import (
	"os"
	"strings"
)

// isMountPoint reports whether path is a mount point per /proc/mounts. This
// reads /proc, not /sys, so it stays on plain os rather than the sysfs service.
func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}
