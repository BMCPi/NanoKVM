// Package rpieeprom is a Go port of the section-table parser and file
// updater in raspberrypi/rpi-eeprom (the `rpi-eeprom-config` script's
// BootloaderImage class). It manipulates the binary RPi 4/5 bootloader
// EEPROM image — the same ~2MB blob that `rpi-eeprom-update` flashes —
// so callers can read or replace the embedded modifiable files
// (bootconf.txt, bootcode.bin, pubkey.bin, etc.) without rebuilding the
// image from scratch.
//
// We do NOT flash the EEPROM ourselves: the device-runtime EEPROM update
// path goes through `rpi-eeprom-update`, which expects an updated image
// staged on the boot FAT (commonly `pieeprom.upd`). This package produces
// that staged image; the host OS handles the actual flash on next boot.
//
// The binary layout (matches the upstream python reference):
//
//	+-------------------+
//	| section 0 header  |  magic(4 BE) | length(4 BE)
//	| section 0 content |  length bytes (8-byte aligned)
//	+-------------------+
//	| section 1 header  |
//	| ...               |
//
// File sections (FileMagic) carry a 12-byte null-padded filename in their
// first 12 content bytes (so FileHdrLen = 4 + 4 + 12 = 20). Padding sections
// (PadMagic) are written by the updater to keep section offsets stable when
// a file shrinks. The trailing 4KB of the image is reserved scratch for the
// bootloader and must not be overwritten.
//
// What this package does NOT cover (yet — extend as needed):
//   - extract_files (trivial wrapper over GetFile across all sections)
//   - set_timestamp
//   - update_key (signed-boot RSA pub key conversion via Cryptodome)
package rpieeprom
