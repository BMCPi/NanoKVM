package ipmi

import (
	"context"
	"encoding/binary"

	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
)

// firmwareStatus is the slice of firmware.Controller the OEM handler needs.
type firmwareStatus interface {
	GetStatus() firmware.Status
}

// OEM command set, NetFn 0x30. The previous server injected the firmware
// controller but never dispatched to it; this is the surface that plumbing
// was for.
const (
	oemNetFn types.NetFn = 0x30

	// oemCmdGetFirmwareStatus reports the capsule-update state.
	//
	// Request:  (empty)
	// Response: flags(1)  bit0 = capsule volume ready on disk
	//                     bit1 = volume presented on the USB gadget lun
	//                     bit2 = a capsule fetch/stage is in progress
	//           size(4)   capsule volume size in KiB, little-endian
	//
	// e.g. ipmitool raw 0x30 0x01
	oemCmdGetFirmwareStatus uint8 = 0x01
)

func registerOEMHandlers(reg *handlers.Registry, fw firmwareStatus) {
	reg.RegisterFunc(
		types.Command{ID: oemCmdGetFirmwareStatus, NetFn: oemNetFn, Name: "OEM Get Firmware Status"},
		func(ctx context.Context, hctx *handlers.HandlerContext, req []byte) ([]byte, types.CompletionCode, error) {
			st := fw.GetStatus()

			var flags byte
			if st.VolumeReady {
				flags |= 1 << 0
			}
			if st.Presented {
				flags |= 1 << 1
			}
			if st.Staging {
				flags |= 1 << 2
			}

			resp := make([]byte, 5)
			resp[0] = flags
			binary.LittleEndian.PutUint32(resp[1:], uint32(st.VolumeSize/1024)) //nolint:gosec // KiB of a <=64 MiB volume
			return resp, types.CodeOK, nil
		})
}
