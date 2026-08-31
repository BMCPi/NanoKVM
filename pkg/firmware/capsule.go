package firmware

// capsule.go owns the FMP capsule volume: a GPT disk image holding a single
// EFI System Partition formatted FAT32, whose \EFI\UpdateCapsule\ directory is
// the drop box the host's firmware scans at boot (UEFI 2.10 §8.5.5,
// "Delivering Capsules Across a System Reset").
//
// Everything here runs in userspace via go-diskfs — the image is never loop
// mounted, so no root-only mount/umount/drop_caches cycle is involved and the
// BMC's view of the volume can never diverge from what it wrote. The gadget is
// unpresented around every mutation so the host cannot observe a half-written
// FAT, and re-presented after so it sees the change as media insertion.
//
// Reads open the image fresh every time on purpose: host firmware deletes each
// capsule from the directory once it has applied it, so a cached FAT view
// would report applied capsules as still pending.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

const (
	// capsuleDir is the directory inside the ESP that firmware scans, spelled
	// the way the UEFI spec does. Reported to operators; never passed to
	// go-diskfs.
	capsuleDir = `\EFI\UpdateCapsule`

	// capsuleDirPath is that same directory as go-diskfs wants it: an
	// io/fs-valid path, so slash-separated AND unrooted. FileSystem.ReadDir
	// runs fs.ValidPath and rejects a leading slash, while Mkdir/OpenFile/
	// Remove accept either — using the strict form everywhere keeps the two
	// from drifting apart.
	capsuleDirPath = "EFI/UpdateCapsule"

	// capsuleExt is appended to staged names that carry no extension. Capsule
	// files are conventionally *.cap; firmware reads every file in the
	// directory regardless, so this is cosmetic but keeps the volume legible.
	capsuleExt = ".cap"

	// espFirstLBA is where the EFI System Partition starts: the conventional
	// 1 MiB alignment at 512-byte sectors.
	espFirstLBA = 2048
	// gptTailSectors is what GPT reserves at the end of the disk: the backup
	// header plus its 32-sector partition entry array, plus the sector the
	// header itself occupies.
	gptTailSectors = 34
	// sectorSize is the logical sector size of the capsule volume, matching
	// what f_mass_storage exposes for a file-backed LUN.
	sectorSize = 512

	// capsuleMinSizeMB floors the volume size: FAT32 is only well-formed at
	// >= 65525 clusters, and at 512-byte clusters that cliff sits at ~33 MB, so
	// 48 MiB (~98k clusters) clears it comfortably.
	//
	// capsuleMaxSizeMB caps it at 256 MiB, which is both far more than any
	// realistic set of firmware capsules needs and the point below which
	// go-diskfs formats FAT32 with one-sector clusters — the geometry
	// fatLayoutIsConsistent reasons about.
	capsuleMinSizeMB = 48
	capsuleMaxSizeMB = 256

	// FAT32 layout constants, mirroring what go-diskfs's fat32.Create lays
	// down. They exist here so espSectorCount can predict the geometry it is
	// about to ask for; see fatLayoutIsConsistent.
	fatReservedSectors   = 32 // boot sector, FSInfo, their backups, padded
	fatCopies            = 2
	fatEntryBytes        = 4
	fatSectorsPerCluster = 1 // 512-byte clusters, for volumes under 260 MB

	// FAT directory-entry layout, for the \EFI ".." repair below.
	dirEntrySize      = 32
	dirEntryNameLen   = 11
	dirEntryAttrOff   = 11
	dirEntryClusHIOff = 20
	dirEntryClusLOOff = 26
	dirAttrDirectory  = 0x10
	dirAttrLongName   = 0x0f
	// fat32RootCluster is where a FAT32 root directory lives, and the value a
	// child's ".." must NOT carry (see fixRootChildDotDot).
	fat32RootCluster = 2

	// capsuleMaxNameLen bounds a staged filename. FAT long names allow 255;
	// this leaves room for the directory prefix and keeps `ls` output sane.
	capsuleMaxNameLen = 200

	// volumeLabel is the FAT volume label. Operators see it in the host OS
	// when the gadget is mounted there.
	volumeLabel = "CAPSULE"
)

// stagingSentinel marks an in-flight capsule fetch. A file rather than a field
// so a crashed fetch does not wedge the controller across a restart: /tmp is a
// RAM overlay on this board and is empty at boot.
const stagingSentinel = "/tmp/.capsule_staging_in_progress"

// Capsule describes one staged capsule as the host firmware will find it.
type Capsule struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitempty"`
}

// capsuleVolumeBytes converts a configured size in MiB to bytes, clamped to
// the range FAT32 and this board can actually carry.
func capsuleVolumeBytes(sizeMB int) int64 {
	if sizeMB < capsuleMinSizeMB {
		sizeMB = capsuleMinSizeMB
	}
	if sizeMB > capsuleMaxSizeMB {
		sizeMB = capsuleMaxSizeMB
	}
	return int64(sizeMB) * 1024 * 1024
}

// isStaging reports whether a capsule fetch is in progress.
func isStaging() bool {
	_, err := os.Stat(stagingSentinel)
	return err == nil
}

// ---------------------------------------------------------------------------
// Volume lifecycle
// ---------------------------------------------------------------------------

// volumeSize returns the capsule volume's size on disk. Must hold c.mu.
func (c *Controller) volumeSize() (int64, bool) {
	info, err := os.Stat(c.capsulePath)
	if err != nil || info.Size() == 0 {
		return 0, false
	}
	return info.Size(), true
}

// ensureVolumeExistsLocked creates the capsule volume if it is not there yet.
// Must hold c.mu.
func (c *Controller) ensureVolumeExistsLocked() error {
	if c.capsulePath == "" {
		return fmt.Errorf("capsulePath not configured")
	}
	if _, ok := c.volumeSize(); ok {
		return nil
	}
	return c.createVolumeLocked()
}

// ensureVolumeLocked creates the capsule volume when it is missing and makes
// sure \EFI\UpdateCapsule\ exists on it, so an operator who mounts the gadget
// finds the drop box even before anything is staged. Idempotent.
// Must hold c.mu.
func (c *Controller) ensureVolumeLocked() error {
	if _, ok := c.volumeSize(); !ok {
		return c.ensureVolumeExistsLocked()
	}

	// The volume survives restarts, but a host that applied every capsule may
	// have removed the directory along with the files it held. Probe read-only
	// first so the common case costs no gadget cycle, and only take the write
	// path (which unpresents lun.0) when the directory is actually gone.
	present := false
	if err := c.withVolume(false, func(fs filesystem.FileSystem) error {
		if _, err := fs.ReadDir(capsuleDirPath); err == nil {
			present = true
		} else if !isFATNotFound(err) {
			return fmt.Errorf("read %s: %w", capsuleDir, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if present {
		return nil
	}
	return c.withVolume(true, ensureCapsuleDir)
}

// createVolumeLocked writes a fresh GPT + ESP capsule volume at c.capsulePath.
// The image is built at a temporary path and renamed into place, so a power
// cut cannot leave a half-formatted volume that volumeSize() would accept.
// Must hold c.mu.
func (c *Controller) createVolumeLocked() error {
	if err := os.MkdirAll(path.Dir(c.capsulePath), 0o755); err != nil {
		return fmt.Errorf("create capsule dir: %w", err)
	}

	tmp := c.capsulePath + ".tmp"
	// diskfs.Create refuses an existing path; clear any leftover from a
	// previous interrupted run.
	_ = os.Remove(tmp)
	defer os.Remove(tmp)

	c.log.Info("firmware: creating capsule volume", slog.Int64("sizeMB", c.capsuleSize/(1024*1024)), slog.String("path", c.capsulePath))

	d, err := diskfs.Create(tmp, c.capsuleSize, diskfs.SectorSizeDefault)
	if err != nil {
		return fmt.Errorf("create capsule volume: %w", err)
	}

	if err := writeCapsuleVolume(d, c.capsuleSize); err != nil {
		_ = d.Close()
		return err
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close capsule volume: %w", err)
	}

	if err := os.Rename(tmp, c.capsulePath); err != nil {
		return fmt.Errorf("install capsule volume: %w", err)
	}
	syncVolume(c.log, c.capsulePath)
	c.log.Info("firmware: capsule volume ready", slog.String("path", c.capsulePath), slog.String("dir", capsuleDir))
	return nil
}

// writeCapsuleVolume lays a protective MBR + GPT with one EFI System Partition
// over d, formats that partition FAT32 and creates the capsule directory.
// The ESP type GUID matters: firmware locates the capsule drop box by walking
// EFI System Partitions, not by filesystem type alone.
func writeCapsuleVolume(d *disk.Disk, size int64) error {
	if size <= 0 {
		return fmt.Errorf("capsule volume size %d is not positive", size)
	}
	// Guarded positive above and clamped by capsuleVolumeBytes to at most
	// capsuleMaxSizeMB, so neither conversion below can wrap.
	totalSectors := uint64(size) / sectorSize
	// Last LBA the GPT leaves usable, i.e. just before the backup header and
	// its partition entry array.
	lastUsable := totalSectors - gptTailSectors
	espSectors := espSectorCount(lastUsable - espFirstLBA + 1)
	if espSectors == 0 {
		return fmt.Errorf("capsule volume of %d bytes is too small for an EFI System Partition", size)
	}

	table := &gpt.Table{
		LogicalSectorSize:  sectorSize,
		PhysicalSectorSize: sectorSize,
		ProtectiveMBR:      true,
		Partitions: []*gpt.Partition{{
			Index: 1,
			Start: espFirstLBA,
			End:   espFirstLBA + espSectors - 1,
			Type:  gpt.EFISystemPartition,
			Name:  "EFI System Partition",
		}},
	}
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("write capsule GPT: %w", err)
	}

	fs, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   1,
		FSType:      filesystem.TypeFat32,
		VolumeLabel: volumeLabel,
	})
	if err != nil {
		return fmt.Errorf("format capsule ESP: %w", err)
	}
	if err := ensureCapsuleDir(fs); err != nil {
		return err
	}
	geom, ok := fs.(fatGeometry)
	if !ok {
		return fmt.Errorf("capsule ESP is %T, which exposes no cluster geometry", fs)
	}
	return fixRootChildDotDot(geom, "EFI")
}

// fatGeometry is the cluster arithmetic go-diskfs's FAT filesystems expose.
// Declared as an interface so this file needs no dependency on the concrete
// fat32 type.
type fatGeometry interface {
	Backend() backend.Storage
	Start() int64
	DataStart() uint32
	BytesPerCluster() int
}

// fixRootChildDotDot rewrites the ".." entry of a directory that sits directly
// under the root so it reads cluster 0.
//
// FAT requires ".." to be 0 when it refers to the root, even on FAT32 where the
// root is a real cluster chain starting at 2. go-diskfs writes the literal
// root cluster instead, which fsck.vfat reports as "Invalid '..' entry in the
// second slot" and repairs. Firmware opening \EFI\UpdateCapsule\ by absolute
// path never reads "..", so this is not what would break a capsule update —
// but a volume handed to a pre-boot FAT driver should not be one that fails
// its own filesystem check, and the fix is four bytes.
func fixRootChildDotDot(g fatGeometry, name string) error {
	f, err := g.Backend().Writable()
	if err != nil {
		return fmt.Errorf("capsule ESP is not writable: %w", err)
	}
	clusterAt := func(cluster uint32) int64 {
		return g.Start() + int64(g.DataStart()) + int64(cluster-fat32RootCluster)*int64(g.BytesPerCluster())
	}

	root := make([]byte, g.BytesPerCluster())
	if _, err := f.ReadAt(root, clusterAt(fat32RootCluster)); err != nil {
		return fmt.Errorf("read capsule ESP root directory: %w", err)
	}
	child, ok := findDirEntryCluster(root, name)
	if !ok {
		return fmt.Errorf("directory %q not found in capsule ESP root", name)
	}

	// Slot 0 is ".", slot 1 is ".."; only the latter needs correcting.
	entry := make([]byte, dirEntrySize)
	dotDotAt := clusterAt(child) + dirEntrySize
	if _, err := f.ReadAt(entry, dotDotAt); err != nil {
		return fmt.Errorf("read %q .. entry: %w", name, err)
	}
	if string(entry[:2]) != ".." {
		return fmt.Errorf("%q second directory slot is %q, want a %q entry", name, entry[:dirEntryNameLen], "..")
	}
	if dirEntryCluster(entry) == 0 {
		return nil // already correct; a future go-diskfs may fix this upstream
	}
	var zero [2]byte
	if _, err := f.WriteAt(zero[:], dotDotAt+dirEntryClusHIOff); err != nil {
		return fmt.Errorf("clear %q .. cluster-high: %w", name, err)
	}
	if _, err := f.WriteAt(zero[:], dotDotAt+dirEntryClusLOOff); err != nil {
		return fmt.Errorf("clear %q .. cluster-low: %w", name, err)
	}
	return nil
}

// findDirEntryCluster returns the first cluster of the subdirectory named name
// within a raw FAT directory cluster. Long-name entries are skipped: name is
// matched against the 8.3 short name, which is all we ever create here.
func findDirEntryCluster(dir []byte, name string) (uint32, bool) {
	want := fmt.Sprintf("%-11s", strings.ToUpper(name))
	for off := 0; off+dirEntrySize <= len(dir); off += dirEntrySize {
		e := dir[off : off+dirEntrySize]
		if e[0] == 0x00 {
			break // no further entries in this directory
		}
		if e[0] == 0xe5 || e[dirEntryAttrOff] == dirAttrLongName {
			continue // deleted, or a long-name fragment
		}
		if e[dirEntryAttrOff]&dirAttrDirectory == 0 {
			continue
		}
		if string(e[:dirEntryNameLen]) == want {
			return dirEntryCluster(e), true
		}
	}
	return 0, false
}

// dirEntryCluster reads the split high/low first-cluster fields of a FAT
// directory entry.
func dirEntryCluster(e []byte) uint32 {
	hi := binary.LittleEndian.Uint16(e[dirEntryClusHIOff : dirEntryClusHIOff+2])
	lo := binary.LittleEndian.Uint16(e[dirEntryClusLOOff : dirEntryClusLOOff+2])
	return uint32(hi)<<16 | uint32(lo)
}

// espSectorCount returns how many sectors to give the EFI System Partition,
// given how many the GPT leaves available.
//
// It is not simply "all of them". go-diskfs sizes the FAT with a closed form
// that, at arbitrary partition sizes, can leave the data area one cluster
// longer than the FAT has entries to describe — fsck.vfat reports
// "Filesystem has N clusters but only space for N-1 FAT entries" and mtools
// rejects the volume outright. Handing host firmware a filesystem that fails
// its own consistency check is not worth the one sector it buys, so this walks
// down from the available count and takes the largest size whose layout checks
// out.
func espSectorCount(available uint64) uint64 {
	for n := available; n > fatReservedSectors; n-- {
		if fatLayoutIsConsistent(n) {
			return n
		}
	}
	return 0
}

// fatLayoutIsConsistent reports whether a FAT32 filesystem of n sectors, laid
// out the way go-diskfs lays one out, can address every cluster in its own
// data area. Entries 0 and 1 of a FAT are reserved, so clusters are numbered
// from 2 and the table needs clusters+2 entries.
func fatLayoutIsConsistent(n uint64) bool {
	if n <= fatReservedSectors {
		return false
	}
	// Smallest sectorsPerFat that covers the data area, as fat32.Create
	// computes it: the FAT copies themselves consume space that would
	// otherwise be clusters, hence the +fatCopies*fatEntryBytes term.
	denom := uint64(sectorSize*fatSectorsPerCluster + fatCopies*fatEntryBytes)
	sectorsPerFat := (fatEntryBytes*(n-fatReservedSectors) + denom - 1) / denom

	dataSectors := n - fatReservedSectors
	if dataSectors <= fatCopies*sectorsPerFat {
		return false
	}
	dataSectors -= fatCopies * sectorsPerFat

	clusters := dataSectors / fatSectorsPerCluster
	entries := sectorsPerFat * (sectorSize / fatEntryBytes)
	return clusters > 0 && clusters+2 <= entries
}

// ensureCapsuleDir creates \EFI\UpdateCapsule\ if it is not already there.
func ensureCapsuleDir(fs filesystem.FileSystem) error {
	if err := fs.Mkdir(capsuleDirPath); err != nil {
		return fmt.Errorf("create %s: %w", capsuleDir, err)
	}
	return nil
}

// withVolume opens the capsule volume's ESP and runs fn against it.
//
// When write is true the gadget is unpresented first so the host cannot read a
// half-written FAT, and re-presented afterwards — even on error — so it sees
// the change as a media insertion. Read-only callers skip that cycle: they and
// f_mass_storage go through the same inode, so the kernel keeps them coherent.
// Must hold c.mu.
func (c *Controller) withVolume(write bool, fn func(filesystem.FileSystem) error) error {
	if _, ok := c.volumeSize(); !ok {
		return fmt.Errorf("capsule volume not found: %s", c.capsulePath)
	}

	if write {
		wasPresented := c.presented
		if wasPresented {
			if err := c.unpresentVolume(); err != nil {
				return fmt.Errorf("unpresent gadget: %w", err)
			}
			defer func() {
				if err := c.presentVolume(); err != nil {
					c.log.Warn("firmware: re-present after write failed", slog.Any("err", err))
				}
			}()
		}
	}

	mode := diskfs.ReadOnly
	if write {
		mode = diskfs.ReadWrite
	}
	d, err := diskfs.Open(c.capsulePath, diskfs.WithOpenMode(mode))
	if err != nil {
		return fmt.Errorf("open capsule volume: %w", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			c.log.Warn("firmware: close capsule volume failed", slog.Any("err", err))
		}
		if write {
			syncVolume(c.log, c.capsulePath)
		}
	}()

	fs, err := d.GetFilesystem(1)
	if err != nil {
		return fmt.Errorf("open capsule ESP: %w", err)
	}
	return fn(fs)
}

// ---------------------------------------------------------------------------
// Capsule operations
// ---------------------------------------------------------------------------

// capsuleFileName normalises a caller-supplied capsule name to a bare FAT
// filename, rejecting anything that could escape the capsule directory.
func capsuleFileName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	if clean == "" || clean == "." || clean == ".." || strings.ContainsAny(clean, `/\`) {
		return "", fmt.Errorf("invalid capsule name %q", name)
	}
	if path.Ext(clean) == "" {
		clean += capsuleExt
	}
	if len(clean) > capsuleMaxNameLen {
		return "", fmt.Errorf("capsule name %q is longer than %d characters", name, capsuleMaxNameLen)
	}
	return clean, nil
}

// ListCapsules returns the capsules currently staged for the host, newest
// first is not attempted — FAT ordering is directory order, which is the order
// firmware will walk them in.
func (c *Controller) ListCapsules() ([]Capsule, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCapsulesLocked()
}

// listCapsulesLocked is ListCapsules without the lock. Must hold c.mu.
func (c *Controller) listCapsulesLocked() ([]Capsule, error) {
	capsules := []Capsule{}
	err := c.withVolume(false, func(fs filesystem.FileSystem) error {
		entries, err := fs.ReadDir(capsuleDirPath)
		if err != nil {
			// A host that applied everything may have removed the directory
			// along with its contents; that is "no capsules", not an error.
			if isFATNotFound(err) {
				return nil
			}
			return fmt.Errorf("read %s: %w", capsuleDir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			entry := Capsule{Name: e.Name()}
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
				entry.Modified = info.ModTime()
			}
			capsules = append(capsules, entry)
		}
		return nil
	})
	return capsules, err
}

// StageCapsule streams r into \EFI\UpdateCapsule\<name> on the capsule volume
// and returns the number of bytes written. The host applies it at its next
// boot; nothing is flashed here.
func (c *Controller) StageCapsule(name string, r io.Reader) (int64, error) {
	fileName, err := capsuleFileName(name)
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Only the volume itself has to pre-exist; the write below recreates the
	// capsule directory if a host that applied everything removed it, so
	// there is no point paying for a second gadget cycle to assert it here.
	if err := c.ensureVolumeExistsLocked(); err != nil {
		return 0, err
	}

	var written int64
	err = c.withVolume(true, func(fs filesystem.FileSystem) error {
		if err := ensureCapsuleDir(fs); err != nil {
			return err
		}
		target := capsuleDirPath + "/" + fileName
		f, err := fs.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
		if err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}
		written, err = io.Copy(f, r)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			// Leave nothing half-written for firmware to choke on.
			_ = fs.Remove(target)
			return fmt.Errorf("write %s: %w", target, err)
		}
		c.log.Info("firmware: staged capsule", slog.String("path", target), slog.Int64("bytes", written))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// RemoveCapsule deletes a staged capsule before the host has consumed it.
// Removing one the host already applied is a no-op: firmware deletes each
// capsule itself once it is applied.
func (c *Controller) RemoveCapsule(name string) error {
	fileName, err := capsuleFileName(name)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.withVolume(true, func(fs filesystem.FileSystem) error {
		target := capsuleDirPath + "/" + fileName
		if err := fs.Remove(target); err != nil {
			if isFATNotFound(err) {
				return nil
			}
			return fmt.Errorf("remove %s: %w", target, err)
		}
		c.log.Info("firmware: removed staged capsule", slog.String("path", target))
		return nil
	})
}

// ClearCapsules removes every staged capsule, cancelling a pending update.
func (c *Controller) ClearCapsules() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.withVolume(true, func(fs filesystem.FileSystem) error {
		entries, err := fs.ReadDir(capsuleDirPath)
		if err != nil {
			if isFATNotFound(err) {
				return nil
			}
			return fmt.Errorf("read %s: %w", capsuleDir, err)
		}
		removed := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			target := capsuleDirPath + "/" + e.Name()
			if err := fs.Remove(target); err != nil && !isFATNotFound(err) {
				return fmt.Errorf("remove %s: %w", target, err)
			}
			removed++
		}
		c.log.Info("firmware: cleared staged capsules", slog.Int("count", removed))
		return nil
	})
}

// isFATNotFound reports whether err means "no such entry in the FAT".
// go-diskfs returns plain formatted errors for this rather than fs.ErrNotExist.
func isFATNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found")
}

// syncVolume flushes the capsule volume to stable storage so a power cut
// between staging a capsule and the host's reboot cannot lose it. Only this
// one file is synced — f_mass_storage reads the same inode through the same
// page cache, so nothing global is needed for the host to see fresh bytes.
func syncVolume(log *slog.Logger, volumePath string) {
	f, err := os.Open(volumePath)
	if err != nil {
		log.Warn("firmware: open capsule volume to sync failed", slog.Any("err", err))
		return
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		log.Warn("firmware: sync capsule volume failed", slog.Any("err", err))
	}
}
