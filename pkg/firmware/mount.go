package firmware

// mount.go provides read/write access to the GPT-partitioned firmware image.
//
// The image uses a GPT partition table with the EFI/FAT partition starting at
// LBA 2048 (offset = 2048 × 512 = 1 048 576 bytes). The kernel's loop-mount
// support (mount -o loop,offset=...) handles both loop attachment and FAT
// mounting in a single call, so no explicit losetup management is required.
//
// Mount cycle: unpresent gadget → mount → fn() → sync → umount →
//              drop_caches → present gadget.
//
// All of these mutate controller state and must run with c.mu held. The
// public Controller methods take the lock.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	log "github.com/sirupsen/logrus"
)

// partitionOffset is the byte offset of the first (EFI/FAT) partition inside
// the image: LBA 2048 × 512 bytes/sector.
const partitionOffset = 2048 * 512

// withMount runs fn with c.imagePath mounted read/write at c.mountPoint.
// The USB gadget is unpresented for the duration and re-presented after,
// even on error. Must be called with c.mu held.
func (c *Controller) withMount(fn func() error) error {
	if !c.imageExists() {
		return fmt.Errorf("firmware image not found: %s", c.imagePath)
	}

	wasPresented := c.presented
	if wasPresented {
		if err := c.unpresentImage(); err != nil {
			return fmt.Errorf("unpresent gadget: %w", err)
		}
	}
	defer func() {
		if wasPresented {
			if err := c.presentImage(); err != nil {
				log.Warnf("firmware: re-present after mount failed: %v", err)
			}
		}
	}()

	if err := c.mountLocked(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer func() {
		if err := c.unmountLocked(); err != nil {
			log.Warnf("firmware: deferred unmount failed: %v", err)
		}
	}()

	return fn()
}

// mountLocked mounts the FAT partition of c.imagePath at c.mountPoint using
// the kernel's built-in loop-mount with a partition byte offset.
// Idempotent. Must hold c.mu.
func (c *Controller) mountLocked() error {
	if c.mountPoint == "" {
		return fmt.Errorf("mountPoint not configured")
	}
	if err := os.MkdirAll(c.mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	if isMounted(c.mountPoint) {
		return nil
	}

	opts := fmt.Sprintf("rw,sync,loop,offset=%d", partitionOffset)
	out, err := exec.Command("mount", "-t", "vfat", "-o", opts, c.imagePath, c.mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %s: %w", c.imagePath, strings.TrimSpace(string(out)), err)
	}
	log.Debugf("firmware: mounted %s (offset=%d) at %s", c.imagePath, partitionOffset, c.mountPoint)
	return nil
}

// unmountLocked unmounts c.mountPoint and flushes caches so the gadget
// re-reads fresh bytes from disk on its next access. Must hold c.mu.
func (c *Controller) unmountLocked() error {
	if !isMounted(c.mountPoint) {
		return nil
	}
	// Flush dirty pages so the on-disk image reflects this write window.
	_ = exec.Command("sync").Run()

	out, err := exec.Command("umount", c.mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Invalidate page cache so f_mass_storage doesn't serve stale pages
	// the next time the host issues a READ on the gadget LUN.
	_ = os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644)

	log.Debug("firmware: unmounted and flushed caches")
	return nil
}

// isMounted checks /proc/mounts for the given path.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}
