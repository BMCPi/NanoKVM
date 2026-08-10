package smbios

// store.go wires the singleton Store from config. It reuses the efivars
// backends - they address the device with absolute offsets and satisfy
// Backend structurally - so all three stores share one EEPROM device.

import (
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
)

var (
	instance *Store
	once     sync.Once
)

// GetStore returns the singleton Store, initializing it from config on first
// call. The store is non-nil even when unconfigured; use Available.
func GetStore() *Store {
	once.Do(func() {
		cfg := config.GetInstance().SMBIOS
		instance = &Store{}
		if !cfg.Enabled {
			return
		}

		var b Backend
		switch {
		case cfg.Path != "":
			b = efivars.NewFileBackend(cfg.Path, cfg.Offset+cfg.Size)
			log.Infof("smbios: using file store %s at offset %#x", cfg.Path, cfg.Offset)
		case cfg.I2CBus >= 0:
			b = efivars.NewI2CBackend(cfg.I2CBus, uint16(cfg.I2CAddr), //nolint:gosec // 7-bit address
				cfg.PageSize, cfg.Offset+cfg.Size)
			log.Infof("smbios: using i2c store bus %d addr %#x at offset %#x",
				cfg.I2CBus, cfg.I2CAddr, cfg.Offset)
		default:
			log.Warn("smbios: enabled but neither path nor i2c bus configured")
			return
		}

		instance.backend = b
		instance.offset = cfg.Offset
		instance.size = cfg.Size
	})
	return instance
}
