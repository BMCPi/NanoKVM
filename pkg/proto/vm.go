package proto

type IP struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Version string `json:"version"`
	Type    string `json:"type"`
}

type GetInfoRsp struct {
	IPs         []IP   `json:"ips"`
	Mdns        string `json:"mdns"`
	Image       string `json:"image"`
	Application string `json:"application"`
	DeviceKey   string `json:"deviceKey"`
}

type GetHardwareRsp struct {
	Version string `json:"version"`
}

type SetGpioReq struct {
	Action string `validate:"required"` // on / off / reset / forceoff
}

type GetGpioRsp struct {
	PWR bool `json:"pwr"` // power led
	HDD bool `json:"hdd"` // hdd led
}

type GetVirtualDeviceRsp struct {
	Network bool `json:"network"`
	Media   bool `json:"media"`
	Disk    bool `json:"disk"`
}

type UpdateVirtualDeviceReq struct {
	Device string `validate:"required"`
}

type UpdateVirtualDeviceRsp struct {
	On bool `json:"on"`
}

type GetSSHStateRsp struct {
	Enabled bool `json:"enabled"`
}

// GetSSHKeysRsp carries the authorized_keys content, empty when no client keys
// are configured.
type GetSSHKeysRsp struct {
	SSHKey string `json:"sshKey"`
}

// SetSSHKeysReq replaces authorized_keys wholesale. An empty value clears it.
type SetSSHKeysReq struct {
	SSHKey string `json:"sshKey" validate:"omitempty"`
}

type SetTLSReq struct {
	Enabled bool `validate:"omitempty"`
}
