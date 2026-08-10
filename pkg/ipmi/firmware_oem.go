package ipmi

// firmware_oem.go implements custom OEM IPMI commands for U-Boot
// firmware management:
//
//   NetFn 0x30 (OEM)
//     Cmd 0x01 — Get U-Boot Version
//                Response: cc, updateAvailable(0|1), currentLen, current[],
//                          latestLen, latest[]
//     Cmd 0x02 — Trigger U-Boot Update (latest from pi-bmc/firmware-images)
//                Response: cc only (started asynchronously)
//
// Strings are ASCII, length-prefixed (single byte), max 255 bytes.

import (
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

const (
	netFnOEMReq  byte = 0x30
	netFnOEMResp byte = 0x31

	cmdOEMGetUBootVersion byte = 0x01
	cmdOEMUpdateUBoot     byte = 0x02
)

func handleOEMGetUBootVersion() []byte {
	ctrl := firmware.GetController()
	info, err := ctrl.GetUBootVersionInfo()
	if err != nil {
		log.Debugf("IPMI OEM: GetUBootVersionInfo: %v", err)
	}
	cur := truncate255(info.Current)
	lat := truncate255(info.Latest)

	resp := make([]byte, 0, 4+len(cur)+len(lat))
	resp = append(resp, ccOK)
	if info.UpdateAvailable {
		resp = append(resp, 0x01)
	} else {
		resp = append(resp, 0x00)
	}
	resp = append(resp, byte(len(cur)))
	resp = append(resp, []byte(cur)...)
	resp = append(resp, byte(len(lat)))
	resp = append(resp, []byte(lat)...)
	return resp
}

func handleOEMUpdateUBoot() []byte {
	ctrl := firmware.GetController()
	if ctrl.IsDownloading() {
		// Use generic "node busy" completion code.
		return []byte{0xC0}
	}
	go func() {
		if err := ctrl.UpdateUBoot(); err != nil {
			log.Errorf("IPMI OEM: u-boot update failed: %v", err)
		}
	}()
	log.Info("IPMI OEM: u-boot update started")
	return []byte{ccOK}
}

func truncate255(s string) string {
	if len(s) > 255 {
		return s[:255]
	}
	return s
}
