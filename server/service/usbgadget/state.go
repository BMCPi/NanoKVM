package usbgadget

import (
	"encoding/json"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/server/config"
)

// State is the snapshot of the function toggles reported by Gadget.State() and
// consumed by the virtual-device API. It is derived from config now; the JSON
// tags remain because it is also the on-disk shape of the legacy runtime state
// file that migrateLegacyState folds in once on upgrade.
type State struct {
	Ethernet string `json:"ethernet"` // "off" | "ecm" | "ncm"
	Disk     bool   `json:"disk"`     // whether mass_storage.disk0 is linked into c.1
}

// loadLegacyState reads the pre-config runtime state file. ok is false when the
// file is absent (the common case: fresh device, or already migrated) or
// unreadable/corrupt.
func loadLegacyState(path string) (State, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		log.Warnf("usbgadget: corrupt legacy state file %s: %v", path, err)
		return State{}, false
	}
	return s, true
}

// applyLegacyState folds a legacy runtime State into the gadget config. Pure, so
// it is unit-tested. An unrecognized ethernet value is ignored (the config value
// is kept); disk is copied verbatim.
func applyLegacyState(cur config.UsbGadget, s State) config.UsbGadget {
	switch s.Ethernet {
	case EthernetOff, EthernetECM, EthernetNCM:
		cur.Ethernet = s.Ethernet
	}
	cur.Disk = s.Disk
	return cur
}

// migrateLegacyState performs the one-time upgrade from the legacy runtime state
// file to the gadget config: it folds ethernet/disk into config, persists the
// config, and removes the file so it never runs again. A device that never had
// the file (fresh, or a rollback that predates it) is untouched and keeps its
// config values. No lock is taken here — Init calls this while holding g.mu.
func migrateLegacyState() {
	s, ok := loadLegacyState(legacyStatePath)
	if !ok {
		return
	}

	inst := config.GetInstance()
	inst.UsbGadget = applyLegacyState(inst.UsbGadget, s)
	config.Save()

	if err := os.Remove(legacyStatePath); err != nil {
		log.Warnf("usbgadget: remove legacy state file %s: %v", legacyStatePath, err)
	}
	log.Infof("usbgadget: folded legacy state into config (ethernet=%s disk=%v)",
		inst.UsbGadget.Ethernet, inst.UsbGadget.Disk)
}
