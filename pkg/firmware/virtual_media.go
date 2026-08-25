package firmware

// virtual_media.go implements virtual media management.
//
// Workflow:
//   1. One or more ISO files are staged in c.mediaDir (upload / fetch).
//   2. The user selects one to insert.  InsertVirtualMedia writes the staged
//      file path directly to the USB gadget's lun.1 configfs attribute.
//      The managed server sees a USB CD-ROM without the ISO being copied
//      anywhere — no writes to the firmware FAT image at all.
//   3. Ejecting clears lun.1/file so the host sees an empty tray.
//
// This avoids the "no space left" problem of trying to write an ISO into the
// small FAT partition that is already full with U-Boot firmware files.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
)

// Hybrid image conversion constants.
//
// We always present the resulting image with 512-byte logical sectors. That
// matches the MBR convention regardless of the ISO's 2 KiB logical block
// size, and is what `f_mass_storage` exposes to the host.
//
// The image size and the El Torito / hybrid-partition overlay region are
// derived from the ISO at runtime — see hybridParamsFromISO. The previous
// hard-coded recipe corresponded to one specific NoCloud arm64 image:
//
//	dd if=/dev/zero of=out.img bs=1M count=1024
//	dd if=in.iso  of=out.img conv=notrunc
//	dd if=in.iso  of=out.img skip=356 count=5760 bs=512
//
// Now `skip` and `count` come from the ISO's MBR partition table (the
// embedded EFI System Partition, which is what makes the image directly
// bootable when written at LBA 0), and the image size is the ISO size
// padded up to a megabyte boundary.
const (
	hybridSectorSize    = 512
	mbrPartTableOffset  = 446 // bytes
	mbrPartTableEntries = 4
	mbrPartEntrySize    = 16
	mbrSignatureOffset  = 510
	mbrSignature        = 0xAA55
	mbrTypeEFISystem    = 0xEF
	hybridImageMinSize  = 16 * 1024 * 1024 // 16 MiB floor
	hybridImageAlign    = 1024 * 1024      // 1 MiB granularity
)

// hybridParams describes what buildHybridImage needs to know to build a
// bootable raw disk image from a given ISO.
type hybridParams struct {
	ImageSize        int64 // total size of the output image, bytes
	SectorSize       int64 // logical sector size used for skip/count math
	OverlaySkipSects int64 // input offset, in sectors, for the LBA-0 overlay
	OverlayCountSect int64 // overlay length, in sectors
}

// VirtualMediaState describes the current ISO insertion state.
type VirtualMediaState struct {
	Inserted  bool   `json:"inserted"`
	ImageName string `json:"imageName,omitempty"` // original filename chosen by the user
	ImageSize int64  `json:"imageSize,omitempty"` // size in bytes

	// Ephemeral means the staged file's lifecycle is the mount: ejecting
	// deletes it from the media directory. Set for inserts through interfaces
	// with no library view and no delete verb (Redfish), where ejected media
	// that silently accumulates on the data partition could never be cleaned
	// up by the same client that put it there.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// VMGadget is the slice of the USB gadget surface the virtual-media workflow
// drives. Production code always goes through the usbgadget singleton; tests
// inject a fake, because configfs is absent off-device and insert/eject cycles
// (and the ephemeral deletion that hangs off eject) would otherwise be
// untestable.
type VMGadget interface {
	InsertMedia(path string) error
	EjectMedia() error
	LUN1File() (string, bool)
}

func (c *Controller) vmGadget() VMGadget {
	if c.gadget != nil {
		return c.gadget
	}
	return usbgadget.Get()
}

// SetVMGadgetForTest routes the controller's virtual-media gadget calls
// through g. Test-only; call before any media operation.
func (c *Controller) SetVMGadgetForTest(g VMGadget) { c.gadget = g }

// ephemeralMarkerPath is the sidecar recording that name was inserted with the
// ephemeral contract. A file rather than memory because the insertion itself
// outlives the process — configfs keeps lun.1 populated across a BMC restart,
// so the promise to delete on eject has to survive one too.
func (c *Controller) ephemeralMarkerPath(name string) string {
	return filepath.Join(c.mediaDir, "."+name+".ephemeral")
}

// GetVirtualMediaState returns the current virtual media state.
//
// If the in-memory state shows nothing inserted, the lun.1/file configfs
// attribute is consulted as a source of truth. configfs persists across BMC
// restarts, so a previously-inserted ISO must still be reflected in the UI
// after the server process is restarted.
func (c *Controller) GetVirtualMediaState() VirtualMediaState {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.vmState.Inserted {
		if recovered, ok := c.recoverVMStateFromGadget(); ok {
			c.vmState = recovered
		}
	}
	return c.vmState
}

// recoverVMStateFromGadget inspects the gadget's lun.1 backing file and, if it
// points at a readable file, returns a populated VirtualMediaState. The
// ephemeral contract is recovered from the on-disk marker. Caller must hold
// c.mu.
func (c *Controller) recoverVMStateFromGadget() (VirtualMediaState, bool) {
	path, ok := c.vmGadget().LUN1File()
	if !ok {
		return VirtualMediaState{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return VirtualMediaState{}, false
	}
	name := filepath.Base(path)
	_, markErr := os.Stat(c.ephemeralMarkerPath(name))
	return VirtualMediaState{
		Inserted:  true,
		ImageName: name,
		ImageSize: info.Size(),
		Ephemeral: markErr == nil,
	}, true
}

// GetMediaDir returns the path to the ISO staging directory.
func (c *Controller) GetMediaDir() string { return c.mediaDir }

// mediaPathFor returns the absolute path of name inside the media dir and
// verifies the result doesn't escape outside it.
func (c *Controller) mediaPathFor(name string) (string, error) {
	clean := filepath.Base(name)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("invalid media filename %q", name)
	}
	return filepath.Join(c.mediaDir, clean), nil
}

// ListMediaFiles returns the filenames present in the media staging directory.
func (c *Controller) ListMediaFiles() ([]string, error) {
	if c.mediaDir == "" {
		return nil, fmt.Errorf("mediaDir not configured")
	}
	entries, err := os.ReadDir(c.mediaDir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// Dotfiles are bookkeeping (ephemeral markers), not media.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// SaveMediaFile writes r to mediaDir/<name>, creating the directory if needed.
// It returns the number of bytes saved.
//
// The file is written to a temporary path first and only renamed to the final
// destination on success, so an existing ISO is never truncated or removed if
// the write fails part-way through. An error is returned immediately if name
// is currently inserted as virtual media — eject it before overwriting.
func (c *Controller) SaveMediaFile(name string, r io.Reader) (int64, error) {
	if c.mediaDir == "" {
		return 0, fmt.Errorf("mediaDir not configured")
	}
	destPath, err := c.mediaPathFor(name)
	if err != nil {
		return 0, err
	}

	// Refuse to overwrite a file that is currently presented to the host.
	c.mu.Lock()
	inserted := c.vmState.Inserted && c.vmState.ImageName == name
	c.mu.Unlock()
	if inserted {
		return 0, fmt.Errorf("cannot overwrite %q: currently inserted; eject first", name)
	}

	if err := os.MkdirAll(c.mediaDir, 0o755); err != nil {
		return 0, fmt.Errorf("create media dir: %w", err)
	}

	// Stage into a sibling temp file so the destination is only replaced
	// atomically (via rename) after a fully-successful write.
	tmpPath := destPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, r)
	syncErr := f.Sync()
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("write media file: %w", copyErr)
	}
	if syncErr != nil {
		_ = os.Remove(tmpPath)
		return n, fmt.Errorf("sync media file: %w", syncErr)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return n, fmt.Errorf("install media file: %w", err)
	}
	log.Infof("firmware: saved media file %q (%d bytes)", name, n)
	return n, nil
}

// DeleteMediaFile removes name from the media staging directory.
// Returns an error if that file is currently inserted.
func (c *Controller) DeleteMediaFile(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vmState.Inserted && c.vmState.ImageName == name {
		return fmt.Errorf("cannot delete %q: currently inserted; eject first", name)
	}
	destPath, err := c.mediaPathFor(name)
	if err != nil {
		return err
	}
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InsertVirtualMedia presents the named ISO from mediaDir as a USB CD-ROM
// via the gadget's lun.1. The ISO is NOT copied anywhere — the gadget reads
// it directly from mediaDir, so no space in the firmware FAT is consumed.
// The file persists across ejects; it belongs to the staging library.
func (c *Controller) InsertVirtualMedia(name string) error {
	return c.insertVirtualMedia(name, false)
}

// InsertVirtualMediaEphemeral inserts name like InsertVirtualMedia but marks
// the staged file transient: ejecting deletes it from the media directory.
// This is the Redfish lifecycle — that API has no library view and no delete
// verb, so for its clients an eject is the last reference to the image.
func (c *Controller) InsertVirtualMediaEphemeral(name string) error {
	return c.insertVirtualMedia(name, true)
}

func (c *Controller) insertVirtualMedia(name string, ephemeral bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.vmState.Inserted {
		return fmt.Errorf("virtual media already inserted (%s); eject first", c.vmState.ImageName)
	}

	srcPath, err := c.mediaPathFor(name)
	if err != nil {
		return err
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("media file %q not found: %w", name, err)
	}

	if err := c.vmGadget().InsertMedia(srcPath); err != nil {
		return fmt.Errorf("insert virtual media: %w", err)
	}

	marker := c.ephemeralMarkerPath(name)
	if ephemeral {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			// Degrade to persistence rather than promising a deletion the
			// next process (or the sweep) would know nothing about.
			log.Warnf("firmware: cannot mark %q ephemeral: %v (file will persist after eject)", name, err)
			ephemeral = false
		}
	} else {
		// A persistent insert of a previously-ephemeral name renews the file's
		// tenure; a stale marker must not delete it on the next eject.
		_ = os.Remove(marker)
	}

	c.vmState = VirtualMediaState{Inserted: true, ImageName: name, ImageSize: info.Size(), Ephemeral: ephemeral}
	log.Infof("firmware: inserted virtual media %q (%d bytes, ephemeral=%t) via lun.1", name, info.Size(), ephemeral)
	return nil
}

// EjectVirtualMedia clears lun.1 so the managed server sees an empty CD-ROM
// tray. An ephemeral insert's staged file is deleted from the media directory
// once the tray is clear.
func (c *Controller) EjectVirtualMedia() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.vmState.Inserted {
		return nil // idempotent
	}

	prev := c.vmState

	if err := c.vmGadget().EjectMedia(); err != nil {
		return fmt.Errorf("eject virtual media: %w", err)
	}

	c.vmState = VirtualMediaState{}
	c.removeEphemeralLocked(prev)
	log.Infof("firmware: ejected virtual media %s", prev.ImageName)
	return nil
}

// removeEphemeralLocked deletes prev's staged file when the insert was marked
// transient, and clears the marker either way. A deletion failure doesn't fail
// the eject — the tray is already empty — and the surviving marker lets the
// startup sweep retry. Caller holds c.mu.
func (c *Controller) removeEphemeralLocked(prev VirtualMediaState) {
	if prev.ImageName == "" {
		return
	}
	if prev.Ephemeral {
		path, err := c.mediaPathFor(prev.ImageName)
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil && !os.IsNotExist(err) {
			log.Warnf("firmware: delete ephemeral media %q: %v", prev.ImageName, err)
			return // keep the marker so the sweep can finish the job
		}
		log.Infof("firmware: deleted ephemeral media %q", prev.ImageName)
	}
	if err := os.Remove(c.ephemeralMarkerPath(prev.ImageName)); err != nil && !os.IsNotExist(err) {
		log.Warnf("firmware: clear ephemeral marker for %q: %v", prev.ImageName, err)
	}
}

// sweepEphemeralMediaLocked finishes any ephemeral eject a crash or restart
// interrupted: a marker whose file is not the currently-inserted image means
// that mount is over, and the bytes' lifecycle ended with it. Runs once at
// startup (after the gadget is up, so an insertion recovered from configfs is
// left alone). Caller holds c.mu.
func (c *Controller) sweepEphemeralMediaLocked() {
	if c.mediaDir == "" {
		return
	}
	if !c.vmState.Inserted {
		if recovered, ok := c.recoverVMStateFromGadget(); ok {
			c.vmState = recovered
		}
	}

	markers, err := filepath.Glob(filepath.Join(c.mediaDir, ".*.ephemeral"))
	if err != nil {
		return
	}
	for _, marker := range markers {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(marker), "."), ".ephemeral")
		if c.vmState.Inserted && c.vmState.ImageName == name {
			continue
		}
		path, err := c.mediaPathFor(name)
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil && !os.IsNotExist(err) {
			log.Warnf("firmware: sweep ephemeral media %q: %v", name, err)
			continue // keep the marker; retry next startup
		}
		_ = os.Remove(marker)
		log.Infof("firmware: swept leftover ephemeral media %q", name)
	}
}

// SweepEphemeralMedia is the exported form of the startup sweep, for callers
// (and tests) outside Init.
func (c *Controller) SweepEphemeralMedia() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepEphemeralMediaLocked()
}

// SaveMediaISO streams a NoCloud / cloud-init style ISO from r into mediaDir,
// then converts it in place to a 1 GiB hybrid raw disk image with a `.img`
// extension. It returns the final image filename (e.g. "nocloud-arm64.img")
// and its size in bytes. The intermediate `.iso` is removed on success.
//
// Use this when the caller knows the source is an ISO that must be presented
// as a writable/bootable block device rather than as a CD-ROM. For straight
// CD-ROM presentation use SaveMediaFile.
func (c *Controller) SaveMediaISO(name string, r io.Reader) (string, int64, error) {
	if c.mediaDir == "" {
		return "", 0, fmt.Errorf("mediaDir not configured")
	}

	// Stage the ISO under its original name so the conversion can read it
	// with positional I/O.
	if _, err := c.SaveMediaFile(name, r); err != nil {
		return "", 0, err
	}

	isoPath, err := c.mediaPathFor(name)
	if err != nil {
		return "", 0, err
	}

	imgName := strings.TrimSuffix(name, filepath.Ext(name)) + ".img"
	imgPath, err := c.mediaPathFor(imgName)
	if err != nil {
		return "", 0, err
	}

	if err := buildHybridImage(isoPath, imgPath); err != nil {
		_ = os.Remove(imgPath)
		return "", 0, fmt.Errorf("convert iso to hybrid img: %w", err)
	}

	// Source ISO no longer needed once the hybrid image exists.
	if err := os.Remove(isoPath); err != nil && !os.IsNotExist(err) {
		log.Warnf("firmware: could not remove staged ISO %q: %v", isoPath, err)
	}

	info, err := os.Stat(imgPath)
	if err != nil {
		return "", 0, err
	}
	log.Infof("firmware: built hybrid image %q (%d bytes) from %q", imgName, info.Size(), name)
	return imgName, info.Size(), nil
}

// buildHybridImage materializes a raw disk image at imgPath whose contents
// are the ISO at isoPath, with the ISO's hybrid-MBR EFI partition overlaid
// at LBA 0. The image size and overlay region are derived from the ISO
// itself by hybridParamsFromISO. This is the pure-Go equivalent of the
// three-step `dd` recipe documented above.
func buildHybridImage(isoPath, imgPath string) error {
	iso, err := os.Open(isoPath)
	if err != nil {
		return err
	}
	defer iso.Close()

	params, err := hybridParamsFromISO(iso)
	if err != nil {
		return fmt.Errorf("derive hybrid params: %w", err)
	}
	log.Infof(
		"firmware: hybrid params for %q: imageSize=%d sectorSize=%d skip=%d count=%d",
		filepath.Base(isoPath), params.ImageSize, params.SectorSize,
		params.OverlaySkipSects, params.OverlayCountSect,
	)

	out, err := os.OpenFile(imgPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	// 1) Sparse allocation sized to the derived image size.
	if err := out.Truncate(params.ImageSize); err != nil {
		return err
	}

	// 2) Copy the entire ISO over the start of the image
	//    (dd if=iso of=img conv=notrunc).
	if _, err := iso.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek iso: %w", err)
	}
	if _, err := io.Copy(&offsetWriter{w: out, off: 0}, iso); err != nil {
		return fmt.Errorf("copy iso body: %w", err)
	}

	// 3) Overlay the hybrid-MBR / EFI partition region back at LBA 0.
	if params.OverlayCountSect > 0 {
		if _, err := iso.Seek(params.OverlaySkipSects*params.SectorSize, io.SeekStart); err != nil {
			return fmt.Errorf("seek iso overlay: %w", err)
		}
		overlayLen := params.OverlayCountSect * params.SectorSize
		n, err := io.CopyN(&offsetWriter{w: out, off: 0}, iso, overlayLen)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("write overlay: %w", err)
		}
		if n < overlayLen {
			log.Warnf("firmware: hybrid overlay short read: wrote %d of %d bytes", n, overlayLen)
		}
	} else {
		log.Warnf("firmware: no overlay partition found in %q; image may not boot as block device", filepath.Base(isoPath))
	}

	return out.Sync()
}

// hybridParamsFromISO inspects the ISO's MBR partition table at LBA 0 and
// returns the parameters needed to build a bootable hybrid image.
//
// The image size is the ISO size, padded up to the next MiB and bounded
// below by hybridImageMinSize. The overlay region is taken from the MBR
// partition that is most useful for booting — preferring an EFI System
// Partition (type 0xEF), then falling back to the largest non-empty entry.
// If no partition table can be parsed, OverlayCountSect is returned as 0
// and the caller emits the ISO straight through with no overlay.
func hybridParamsFromISO(iso *os.File) (hybridParams, error) {
	info, err := iso.Stat()
	if err != nil {
		return hybridParams{}, err
	}

	params := hybridParams{
		ImageSize:  alignUp(info.Size(), hybridImageAlign),
		SectorSize: hybridSectorSize,
	}
	if params.ImageSize < hybridImageMinSize {
		params.ImageSize = hybridImageMinSize
	}

	// Read the first 512-byte sector to inspect the MBR.
	mbr := make([]byte, hybridSectorSize)
	if _, err := iso.ReadAt(mbr, 0); err != nil {
		return params, fmt.Errorf("read mbr: %w", err)
	}

	// Validate the MBR boot signature; if absent, leave overlay zeroed.
	if binary.LittleEndian.Uint16(mbr[mbrSignatureOffset:mbrSignatureOffset+2]) != mbrSignature {
		return params, nil
	}

	type entry struct {
		typ      byte
		startLBA uint32
		sectors  uint32
	}
	var entries []entry
	for i := 0; i < mbrPartTableEntries; i++ {
		off := mbrPartTableOffset + i*mbrPartEntrySize
		e := entry{
			typ:      mbr[off+4],
			startLBA: binary.LittleEndian.Uint32(mbr[off+8 : off+12]),
			sectors:  binary.LittleEndian.Uint32(mbr[off+12 : off+16]),
		}
		if e.typ == 0 || e.sectors == 0 {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return params, nil
	}

	// Prefer an EFI System Partition; otherwise pick the largest entry.
	pick := entries[0]
	pickedEFI := pick.typ == mbrTypeEFISystem
	for _, e := range entries[1:] {
		if e.typ == mbrTypeEFISystem && !pickedEFI {
			pick = e
			pickedEFI = true
			continue
		}
		if pickedEFI {
			continue
		}
		if e.sectors > pick.sectors {
			pick = e
		}
	}
	params.OverlaySkipSects = int64(pick.startLBA)
	params.OverlayCountSect = int64(pick.sectors)
	return params, nil
}

// alignUp returns n rounded up to the nearest multiple of align (align > 0).
func alignUp(n, align int64) int64 {
	if align <= 0 {
		return n
	}
	return ((n + align - 1) / align) * align
}

// offsetWriter wraps a *os.File so io.Copy / io.CopyN write via WriteAt at a
// stable offset, leaving the file's seek position untouched.
type offsetWriter struct {
	w   io.WriterAt
	off int64
}

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.w.WriteAt(p, o.off)
	o.off += int64(n)
	return n, err
}
