package components

import (
	"strings"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// OverviewServer is the Server Information card body: the merged SMBIOS /
// U-Boot-env identity of the managed host. Zero value renders placeholders;
// the /ui/overview/server fragment supplies the populated model.
type OverviewServer struct {
	Board           string
	Vendor          string
	CPU             string
	Memory          string
	Serial          string
	Revision        string
	MAC             string
	InventorySource string
}

// OverviewUpdateCheck is one "current version + latest release" pair. Checked
// is false until a fragment has actually asked upstream, so first paint can
// render the version row without implying "no update available".
type OverviewUpdateCheck struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	Checked         bool
}

// OverviewBios is the BIOS Information card body. The firmware-status half
// is local and cheap, so it is rendered with the page; the boot rows and the
// U-Boot update check arrive with the /ui/overview/bios fragment.
type OverviewBios struct {
	FirmwareStatus string
	DownloadNeeded bool
	Downloading    bool
	UBoot          OverviewUpdateCheck
	DeviceTree     string
	BootTargets    string
	BootOverride   string
	BootMethods    string
}

// OverviewKernel is one row of the kernel-version selector.
type OverviewKernel struct {
	Kernel     string
	UBoot      string
	Downloaded bool
	Active     bool
}

// OverviewKernels is the Kernel Version card body. Selected names the kernel
// whose action buttons show; empty means nothing selected.
type OverviewKernels struct {
	Kernels     []OverviewKernel
	Selected    string
	Downloading bool
}

// SelectedKernel returns the selected row, if any.
func (m OverviewKernels) SelectedKernel() (OverviewKernel, bool) {
	for _, k := range m.Kernels {
		if k.Kernel == m.Selected {
			return k, true
		}
	}
	return OverviewKernel{}, false
}

// OverviewBiosFirstPaint is the local-only BIOS model rendered with the
// page: firmware cache state now, boot rows and update check by fragment.
func OverviewBiosFirstPaint(fw *firmware.Controller) OverviewBios {
	st := fw.GetStatus()
	m := OverviewBios{
		FirmwareStatus: "Not downloaded",
		DownloadNeeded: !st.Downloaded,
		Downloading:    st.Downloading,
	}
	if st.Downloaded {
		m.FirmwareStatus = "Ready"
	}
	if st.Presented {
		m.FirmwareStatus += " · USB Active"
	}
	return m
}

// OverviewKernelsModel builds the kernel card from local state. activeVer ==
// "" is fine — rows just show no ★. selected names the row whose action
// buttons render.
func OverviewKernelsModel(fw *firmware.Controller, selected, activeVer string) OverviewKernels {
	ctrl := fw
	m := OverviewKernels{Selected: selected, Downloading: ctrl.IsDownloading()}
	for _, k := range firmware.KernelVersionsSorted() {
		ub := firmware.KernelUBootMap[k]
		m.Kernels = append(m.Kernels, OverviewKernel{
			Kernel:     k,
			UBoot:      ub,
			Downloaded: ctrl.VersionedImageExists(ub),
			Active: activeVer != "" && strings.EqualFold(
				strings.TrimPrefix(activeVer, "v"), strings.TrimPrefix(ub, "v")),
		})
	}
	return m
}

// OverviewKernelsFirstPaint renders with the page, so it reads only the
// activation-tracking file — the machine.env fallback (which can touch the
// firmware image) is left to the fragment refresh.
func OverviewKernelsFirstPaint(fw *firmware.Controller) OverviewKernels {
	return OverviewKernelsModel(fw, "", fw.ActiveUBootVersion())
}

// versionDisplay normalizes a version to v-prefixed form; "" and "dev" pass
// through so placeholders and dev builds stay honest.
func versionDisplay(v string) string {
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "dev" {
		return v
	}
	return "v" + v
}
