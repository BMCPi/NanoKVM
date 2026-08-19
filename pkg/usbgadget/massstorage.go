package usbgadget

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Inquiry strings written to the LUNs. Preserve the exact spacing (vendor(8) +
// product(16) + revision(4)) — hosts display these and some match on them.
const (
	lun0Inquiry = "NanoKVM Capsule Volume  0100"
	lun1Inquiry = "NanoKVM Virtual Media   0100"
)

// Hybrid-ISO detection: an MBR boot signature (0xAA55) at byte 510 means the
// image carries a partition table and should be presented as a raw block device
// (cdrom=0) rather than an El Torito CD-ROM (cdrom=1).
const (
	hybridSectorSize   = 512
	mbrSignatureOffset = 510
	mbrSignature       = 0xAA55
)

func (g *Gadget) massStoragePath() string {
	return filepath.Join(g.functionsPath(), "mass_storage.disk0")
}
func (g *Gadget) lun0Path() string { return filepath.Join(g.massStoragePath(), "lun.0") }
func (g *Gadget) lun1Path() string { return filepath.Join(g.massStoragePath(), "lun.1") }

// ensureMassStorageFunc creates mass_storage.disk0 with both LUNs. lun.1 (the
// virtual-media CD-ROM) MUST be created before the function is symlinked into
// configs/c.1: on this SG2002 kernel, attaching the function group runs
// fsg_bind(), which sets common->running, after which fsg_lun_make() returns
// EBUSY. On a clean build the function is not yet linked, so this is safe; on a
// recovery path where it is already linked but lun.1 is missing, the UDC is
// unbound first so the kernel releases the function group. Caller holds g.mu.
func (g *Gadget) ensureMassStorageFunc() error {
	dir := g.massStoragePath()
	lun1 := g.lun1Path()
	linked := g.isLinked("mass_storage.disk0")

	if err := g.fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create mass_storage.disk0: %w", err)
	}

	if _, err := g.fs.Stat(lun1); os.IsNotExist(err) {
		if linked && g.udcBound() {
			_ = g.unbindUDCLocked()
			// Release lun.0's backing file so f_mass_storage lets go.
			_ = g.fs.writeAttr(filepath.Join(g.lun0Path(), "file"), "\n")
			time.Sleep(50 * time.Millisecond)
		}
		if err := g.fs.Mkdir(lun1, 0o755); err != nil {
			return fmt.Errorf("create lun.1: %w", err)
		}
		_ = g.fs.writeAttr(filepath.Join(lun1, "cdrom"), "0")
		_ = g.fs.writeAttr(filepath.Join(lun1, "ro"), "1")
		_ = g.fs.writeAttr(filepath.Join(lun1, "removable"), "1")
		_ = g.fs.writeAttr(filepath.Join(lun1, "inquiry_string"), lun1Inquiry)
	}

	// lun.0 (the FMP capsule volume). Leave file empty — firmware.Controller
	// fills it via PresentDisk. The LUN is a writable, non-CD-ROM removable
	// disk: host firmware deletes each capsule from \EFI\UpdateCapsule\ once
	// it has been applied, so it must be able to write back. Diff-guarded so
	// re-running on an already-bound gadget (server restart) doesn't poke the
	// LUN.
	lun0 := g.lun0Path()
	_ = g.fs.writeAttrIfDifferent(filepath.Join(lun0, "cdrom"), "0")
	_ = g.fs.writeAttrIfDifferent(filepath.Join(lun0, "ro"), "0")
	_ = g.fs.writeAttrIfDifferent(filepath.Join(lun0, "removable"), "1")
	_ = g.fs.writeAttrIfDifferent(filepath.Join(lun0, "inquiry_string"), lun0Inquiry)
	return nil
}

// ensureLUN1Locked recreates lun.1 if it went missing after build (unexpected).
// Caller holds g.mu.
func (g *Gadget) ensureLUN1Locked() error {
	lun1 := g.lun1Path()
	if _, err := g.fs.Stat(lun1); err == nil {
		return nil
	}
	if g.udcBound() {
		_ = g.unbindUDCLocked()
		_ = g.fs.writeAttr(filepath.Join(g.lun0Path(), "file"), "\n")
		time.Sleep(50 * time.Millisecond)
	}
	if err := g.fs.Mkdir(lun1, 0o755); err != nil {
		return fmt.Errorf("create lun.1: %w", err)
	}
	_ = g.fs.writeAttr(filepath.Join(lun1, "cdrom"), "0")
	_ = g.fs.writeAttr(filepath.Join(lun1, "ro"), "1")
	_ = g.fs.writeAttr(filepath.Join(lun1, "removable"), "1")
	_ = g.fs.writeAttr(filepath.Join(lun1, "inquiry_string"), lun1Inquiry)
	return g.ensureBindState()
}

// PresentDisk sets lun.0's backing file to path and re-enumerates if the UDC
// is not already bound. path is the BMC's FMP capsule volume — a writable
// removable disk, never a CD-ROM. The byte sequencing here —
// clear-then-retry-on-EBUSY and "rebind only when unbound" — is load-bearing
// and carried over from the retired boot-image transport that used to own it.
func (g *Gadget) PresentDisk(path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	lun0 := g.lun0Path()

	// Clear first, then set to the image path. Retry on EBUSY because
	// in-flight gadget I/O can briefly hold it.
	filePath := filepath.Join(lun0, "file")
	_ = g.fs.writeAttr(filePath, "\n")
	var lastErr error
	for i := 0; i < 10; i++ {
		if err := g.fs.WriteFile(filePath, []byte(path), 0o666); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("write lun.0 file: %w", lastErr)
	}

	// Only rebind if not already bound; a bound kernel picks up the file change.
	if !g.udcBound() {
		if err := g.rebindUDCLocked(); err != nil {
			return fmt.Errorf("rebind UDC: %w", err)
		}
	}
	log.Infof("usbgadget: presented %s on lun.0", path)
	return nil
}

// UnpresentDisk clears lun.0's backing file so the image is no longer held by
// f_mass_storage and is safe for the BMC to rewrite in place.
func (g *Gadget) UnpresentDisk() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.fs.WriteFile(filepath.Join(g.lun0Path(), "file"), []byte("\n"), 0o666); err != nil {
		return fmt.Errorf("clear lun.0 file: %w", err)
	}
	// Give the kernel a moment to drop its hold on the backing file.
	time.Sleep(100 * time.Millisecond)
	log.Info("usbgadget: unpresented lun.0")
	return nil
}

// InsertMedia presents path as a USB CD-ROM via lun.1 (virtual media). The file
// is read directly from its staging location; nothing is copied.
func (g *Gadget) InsertMedia(path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.ensureLUN1Locked(); err != nil {
		return err
	}
	isISO := strings.EqualFold(filepath.Ext(path), ".iso")
	cdromVal := "0"
	if isISO && !isHybridISO(path) {
		cdromVal = "1"
	}
	lun1 := g.lun1Path()
	_ = g.fs.writeAttr(filepath.Join(lun1, "cdrom"), cdromVal)
	_ = g.fs.writeAttr(filepath.Join(lun1, "ro"), "1")
	_ = g.fs.writeAttr(filepath.Join(lun1, "removable"), "1")
	if err := g.fs.WriteFile(filepath.Join(lun1, "file"), []byte(path), 0o666); err != nil {
		return fmt.Errorf("set lun.1 file: %w", err)
	}
	log.Infof("usbgadget: virtual media lun.1 → %s", path)
	return nil
}

// EjectMedia clears lun.1's backing file so the host sees an empty CD-ROM tray.
func (g *Gadget) EjectMedia() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, err := g.fs.Stat(g.lun1Path()); err != nil {
		return nil // lun.1 doesn't exist; nothing to clear
	}
	if err := g.fs.WriteFile(filepath.Join(g.lun1Path(), "file"), []byte("\n"), 0o666); err != nil {
		return fmt.Errorf("clear lun.1 file: %w", err)
	}
	log.Info("usbgadget: virtual media lun.1 cleared")
	return nil
}

// LUN1File returns the current lun.1 backing file path, if any. Used to recover
// virtual-media state across a server restart (configfs persists).
func (g *Gadget) LUN1File() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	data, err := g.fs.ReadFile(filepath.Join(g.lun1Path(), "file"))
	if err != nil {
		return "", false
	}
	p := strings.TrimSpace(string(data))
	if p == "" {
		return "", false
	}
	return p, true
}

// isHybridISO reports whether path carries an MBR boot signature (0xAA55) at
// byte 510. Moved verbatim from firmware; false on any read error so callers
// fall through to the safer cdrom=1 mode.
func isHybridISO(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, hybridSectorSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return false
	}
	return binary.LittleEndian.Uint16(buf[mbrSignatureOffset:mbrSignatureOffset+2]) == mbrSignature
}
