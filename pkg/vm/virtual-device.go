package vm

import (
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
)

// GetVirtualDevice reports which optional USB gadget functions are currently
// exposed to the host: the ethernet function (network), the mass-storage disk,
// and whether virtual media (an ISO on lun.1) is inserted.
func (s *Service) GetVirtualDevice(c *gin.Context) {
	var rsp proto.Response

	st := usbgadget.Get().State()
	media := firmware.GetController().GetVirtualMediaState().Inserted

	rsp.OkRspWithData(c, &proto.GetVirtualDeviceRsp{
		Network: st.Ethernet != usbgadget.EthernetOff,
		Media:   media,
		Disk:    st.Disk,
	})
	log.Debugf("get virtual device success")
}

// UpdateVirtualDevice toggles the ethernet or disk gadget function. The gadget
// package reconciles the configfs topology and re-enumerates the host; the
// choice is persisted so it survives a reboot.
func (s *Service) UpdateVirtualDevice(c *gin.Context) {
	var req proto.UpdateVirtualDeviceReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, "invalid argument")
		return
	}

	gadget := usbgadget.Get()
	st := gadget.State()

	var on bool
	switch req.Device {
	case "network":
		// Toggle the ethernet function on/off. Enabling defaults to CDC-ECM.
		on = st.Ethernet == usbgadget.EthernetOff
		mode := usbgadget.EthernetOff
		if on {
			mode = usbgadget.EthernetECM
		}
		if err := gadget.SetEthernet(mode); err != nil {
			log.Errorf("set ethernet %s failed: %s", mode, err)
			rsp.ErrRsp(c, -3, "operation failed")
			return
		}
	case "disk":
		on = !st.Disk
		if err := gadget.SetDisk(on); err != nil {
			log.Errorf("set disk %v failed: %s", on, err)
			rsp.ErrRsp(c, -3, "operation failed")
			return
		}
	default:
		rsp.ErrRsp(c, -2, "invalid arguments")
		return
	}

	rsp.OkRspWithData(c, &proto.UpdateVirtualDeviceRsp{
		On: on,
	})

	log.Debugf("update virtual device %s success", req.Device)
}
