package bootloader

// store.go wires the singleton Store to the shared UEFI variable store. The
// bootloader variables live in the same I2C EEPROM region efivars manages
// (offset 0), so the Store reads them through efivars.GetManager rather than
// opening the device a second time.

import (
	"sync"

	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
)

var (
	instance *Store
	once     sync.Once
)

// GetStore returns the singleton Store. It is non-nil even when the UEFI
// variable store is unavailable; use Available (or handle ErrNotConfigured).
func GetStore() *Store {
	once.Do(func() {
		instance = NewStore(efivars.GetManager())
	})
	return instance
}
