package firmware

// kernel_map.go defines the mapping from Linux kernel major.minor versions
// to U-Boot release versions used to boot them on Raspberry Pi.
//
// When a user selects a kernel version, the corresponding U-Boot image is
// downloaded from pi-bmc/firmware-images and stored as a versioned image
// alongside the active image (e.g. uboot-v2026.04.img). The active image
// (uboot-rpi.img) can then be swapped to any downloaded version without
// losing env files.

// KernelUBootMap maps Linux kernel major.minor version strings to the
// U-Boot release version that supports them.
var KernelUBootMap = map[string]string{
	"6.18": "v2026.04",
	"6.19": "v2026.07-rc1",
}

// KernelVersionsSorted returns the KernelUBootMap keys in ascending order.
// The order is fixed here rather than derived dynamically so the UI
// always presents versions consistently without a sort import.
func KernelVersionsSorted() []string {
	return []string{"6.18", "6.19"}
}
