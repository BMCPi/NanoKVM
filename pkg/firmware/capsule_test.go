package firmware

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// newTestController builds a Controller pointed at a capsule volume inside t's
// temp dir. presented stays false, so no code path here touches configfs.
func newTestController(t *testing.T) *Controller {
	t.Helper()
	return &Controller{
		capsulePath: filepath.Join(t.TempDir(), "capsules.img"),
		capsuleSize: capsuleVolumeBytes(capsuleMinSizeMB),
	}
}

func TestCapsuleVolumeIsGPTWithESP(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	// A protective MBR must be present at LBA 0, or firmware that probes for a
	// partition table before parsing GPT will skip the volume entirely.
	raw, err := os.Open(c.capsulePath)
	if err != nil {
		t.Fatalf("open volume: %v", err)
	}
	defer raw.Close()
	mbr := make([]byte, 512)
	if _, err := raw.ReadAt(mbr, 0); err != nil {
		t.Fatalf("read MBR: %v", err)
	}
	if got := binary.LittleEndian.Uint16(mbr[510:512]); got != 0xAA55 {
		t.Errorf("MBR signature = %#04x, want 0xaa55", got)
	}

	d, err := diskfs.Open(c.capsulePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer d.Close()

	table, ok := d.Table.(*gpt.Table)
	if !ok {
		t.Fatalf("partition table is %T, want *gpt.Table", d.Table)
	}
	parts := table.Partitions
	// go-diskfs pads the partition array with empty entries; only the first is
	// ours.
	if len(parts) == 0 || parts[0].Type != gpt.EFISystemPartition {
		t.Fatalf("partition 1 type = %v, want EFI System Partition", parts[0].Type)
	}
	if parts[0].Start != espFirstLBA {
		t.Errorf("ESP start LBA = %d, want %d", parts[0].Start, espFirstLBA)
	}

	fs, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem(1): %v", err)
	}
	if _, err := fs.ReadDir(capsuleDirPath); err != nil {
		t.Errorf("ReadDir(%s): %v", capsuleDirPath, err)
	}
}

func TestStageListRemoveCapsule(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	payload := bytes.Repeat([]byte("FMP!"), 4096) // 16 KiB
	n, err := c.StageCapsule("host-firmware.cap", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("StageCapsule: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("staged %d bytes, want %d", n, len(payload))
	}

	capsules, err := c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(capsules) != 1 || capsules[0].Name != "host-firmware.cap" {
		t.Fatalf("ListCapsules = %+v, want one host-firmware.cap", capsules)
	}
	if capsules[0].Size != int64(len(payload)) {
		t.Errorf("capsule size = %d, want %d", capsules[0].Size, len(payload))
	}

	// The bytes must land where firmware looks for them, unmodified.
	got := readVolumeFile(t, c.capsulePath, capsuleDirPath+"/host-firmware.cap")
	if !bytes.Equal(got, payload) {
		t.Errorf("staged capsule content differs from what was written (%d vs %d bytes)", len(got), len(payload))
	}

	if err := c.RemoveCapsule("host-firmware.cap"); err != nil {
		t.Fatalf("RemoveCapsule: %v", err)
	}
	capsules, err = c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules after remove: %v", err)
	}
	if len(capsules) != 0 {
		t.Errorf("ListCapsules after remove = %+v, want empty", capsules)
	}
}

func TestClearCapsules(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}
	for _, name := range []string{"a.cap", "b.cap", "c.cap"} {
		if _, err := c.StageCapsule(name, strings.NewReader(name)); err != nil {
			t.Fatalf("StageCapsule(%s): %v", name, err)
		}
	}
	if err := c.ClearCapsules(); err != nil {
		t.Fatalf("ClearCapsules: %v", err)
	}
	capsules, err := c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(capsules) != 0 {
		t.Errorf("ListCapsules after clear = %+v, want empty", capsules)
	}
}

// A host that applied every capsule deletes the directory along with the files
// it held. That is "nothing pending", not a broken volume: listing must stay
// quiet and the next stage must recreate the directory.
func TestCapsuleDirRecreatedAfterHostRemovesIt(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}

	d, err := diskfs.Open(c.capsulePath, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	fs, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem(1): %v", err)
	}
	if err := fs.Remove(capsuleDirPath); err != nil {
		t.Fatalf("remove %s: %v", capsuleDirPath, err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close disk: %v", err)
	}

	capsules, err := c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules with directory gone: %v", err)
	}
	if len(capsules) != 0 {
		t.Errorf("ListCapsules = %+v, want empty", capsules)
	}

	if _, err := c.StageCapsule("again.cap", strings.NewReader("payload")); err != nil {
		t.Fatalf("StageCapsule after directory removal: %v", err)
	}
	capsules, err = c.ListCapsules()
	if err != nil {
		t.Fatalf("ListCapsules: %v", err)
	}
	if len(capsules) != 1 || capsules[0].Name != "again.cap" {
		t.Errorf("ListCapsules = %+v, want one again.cap", capsules)
	}
}

func TestCapsuleFileName(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "host.cap", want: "host.cap"},
		{in: "  host.cap  ", want: "host.cap"},
		{in: "host", want: "host.cap"}, // extensionless names get .cap
		{in: "host.bin", want: "host.bin"},
		{in: "", wantErr: true},
		{in: ".", wantErr: true},
		{in: "..", wantErr: true},
		{in: "../escape.cap", wantErr: true},
		{in: "sub/dir.cap", wantErr: true},
		{in: `sub\dir.cap`, wantErr: true},
		{in: strings.Repeat("x", capsuleMaxNameLen+1) + ".cap", wantErr: true},
	}
	for _, tt := range tests {
		got, err := capsuleFileName(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("capsuleFileName(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("capsuleFileName(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("capsuleFileName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCapsuleVolumeBytesClamped(t *testing.T) {
	const mib = 1024 * 1024
	if got := capsuleVolumeBytes(0); got != capsuleMinSizeMB*mib {
		t.Errorf("capsuleVolumeBytes(0) = %d, want %d", got, capsuleMinSizeMB*mib)
	}
	if got := capsuleVolumeBytes(capsuleMaxSizeMB * 4); got != capsuleMaxSizeMB*mib {
		t.Errorf("capsuleVolumeBytes(oversize) = %d, want %d", got, capsuleMaxSizeMB*mib)
	}
	if got := capsuleVolumeBytes(64); got != 64*mib {
		t.Errorf("capsuleVolumeBytes(64) = %d, want %d", got, 64*mib)
	}
}

// readVolumeFile reads one file out of the capsule volume's ESP.
func readVolumeFile(t *testing.T, volumePath, name string) []byte {
	t.Helper()
	d, err := diskfs.Open(volumePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer d.Close()
	fs, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem(1): %v", err)
	}
	data, err := fs.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return data
}

// go-diskfs sizes the FAT with a closed form that can leave the data area one
// cluster longer than the FAT can address; espSectorCount exists to step back
// off that. Assert the invariant directly so a change to either side is caught
// here rather than by fsck on a host that is mid-update.
func TestESPSectorCountYieldsAddressableClusters(t *testing.T) {
	for mb := capsuleMinSizeMB; mb <= capsuleMaxSizeMB; mb++ {
		total := uint64(capsuleVolumeBytes(mb)) / sectorSize
		available := total - gptTailSectors - espFirstLBA + 1
		n := espSectorCount(available)
		if n == 0 {
			t.Fatalf("%d MiB: espSectorCount(%d) = 0, want a usable size", mb, available)
		}
		if n > available {
			t.Fatalf("%d MiB: espSectorCount = %d, larger than the %d available", mb, n, available)
		}
		if !fatLayoutIsConsistent(n) {
			t.Errorf("%d MiB: espSectorCount = %d, whose FAT cannot address its own data area", mb, n)
		}
		// Never give up more than a cluster's worth of slack hunting for a
		// consistent size.
		if available-n > 8 {
			t.Errorf("%d MiB: espSectorCount dropped %d sectors, want at most 8", mb, available-n)
		}
	}
}

// FAT requires a ".." that refers to the root to read cluster 0, even on FAT32
// where the root is cluster 2. go-diskfs writes 2; fixRootChildDotDot corrects
// it. Scanned straight out of the image rather than through the production
// cluster arithmetic, so the test fails if that arithmetic drifts.
func TestNoDotDotPointsAtRootCluster(t *testing.T) {
	c := newTestController(t)
	if err := c.ensureVolumeLocked(); err != nil {
		t.Fatalf("ensureVolumeLocked: %v", err)
	}
	img, err := os.ReadFile(c.capsulePath)
	if err != nil {
		t.Fatalf("read volume: %v", err)
	}

	dotDot := append([]byte(".."+strings.Repeat(" ", 9)), dirAttrDirectory)
	found := 0
	for off := 0; off+dirEntrySize <= len(img); off += dirEntrySize {
		if !bytes.Equal(img[off:off+len(dotDot)], dotDot) {
			continue
		}
		found++
		if got := dirEntryCluster(img[off : off+dirEntrySize]); got == fat32RootCluster {
			t.Errorf("`..` entry at offset %#x points at the root cluster %d, want 0", off, got)
		}
	}
	// \EFI and \EFI\UpdateCapsule each carry one.
	if found != 2 {
		t.Errorf("found %d `..` entries, want 2", found)
	}
}
