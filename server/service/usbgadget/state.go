package usbgadget

// State is the snapshot of the function toggles reported by Gadget.State() and
// consumed by the virtual-device API; it is derived from config, which is the
// sole source of truth for the gadget topology. (The pre-config runtime state
// file at /data/usbgadget/state.json and its one-time fold-in are gone: /data
// no longer exists on the squashfs+overlay layout.)
type State struct {
	Ethernet string `json:"ethernet"` // "off" | "ecm" | "ncm"
	Disk     bool   `json:"disk"`     // whether mass_storage.disk0 is linked into c.1
}
