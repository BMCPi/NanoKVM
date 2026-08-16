package cvi

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The out-of-tree media drivers.
//
// Nothing else on the system loads these -- there is no modules-load.d entry
// and no udev rule, because none of them binds to a bus that would trigger
// one. Until they are inserted, /dev/cvi-base and friends do not exist and
// Open cannot get past its first device.
//
// The order is a dependency order, not a preference: sys and base export the
// symbols the rest link against, and vc_driver wants the codec and jpeg
// modules already present. Unloading runs in reverse for the same reason.
var mediaModules = []string{
	"cv181x_sys",
	"cv181x_base",
	"snsr_i2c",
	"cvi_mipi_rx",
	"cv181x_vi",
	"cv181x_vpss",
	"cv181x_vcodec",
	"cv181x_jpeg",
	"cvi_vc_driver",
}

// moduleDirs are searched in order for the .ko files. Which one is real
// depends on how the image was assembled, so both are tried rather than
// baking in an assumption that breaks on the other layout.
var moduleDirs = []string{"/lib/modules", "/usr/lib/modules"}

// loadedModules reports which kernel modules are currently inserted.
//
// /proc/modules names them with underscores regardless of whether the file on
// disk used a dash, which is why lookups here normalise.
func loadedModules() (map[string]bool, error) {
	f, err := os.Open("/proc/modules")
	if err != nil {
		return nil, fmt.Errorf("cvi: read /proc/modules: %w", err)
	}
	defer f.Close()

	out := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, _, ok := strings.Cut(sc.Text(), " "); ok {
			out[normaliseModule(name)] = true
		}
	}
	return out, sc.Err()
}

func normaliseModule(name string) string {
	return strings.ReplaceAll(strings.TrimSuffix(name, ".ko"), "-", "_")
}

// findModule locates a module's .ko for the running kernel.
func findModule(release, name string) (string, error) {
	for _, dir := range moduleDirs {
		base := filepath.Join(dir, release)
		for _, sub := range []string{"extra", "kernel", "."} {
			path := filepath.Join(base, sub, name+".ko")
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("cvi: no %s.ko under %v for kernel %s", name, moduleDirs, release)
}

// loadMediaModules inserts the media drivers that are not already present.
//
// It returns the modules it actually inserted, in load order, so teardown can
// remove exactly those. Anything that was already loaded is left alone in both
// directions: it may have been put there by something else, and taking it out
// from under that would be worse than leaving it.
func loadMediaModules() ([]string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return nil, fmt.Errorf("cvi: uname: %w", err)
	}
	release := unix.ByteSliceToString(uts.Release[:])

	present, err := loadedModules()
	if err != nil {
		return nil, err
	}

	var inserted []string
	for _, name := range mediaModules {
		if present[normaliseModule(name)] {
			continue
		}

		path, err := findModule(release, name)
		if err != nil {
			return inserted, err
		}
		f, err := os.Open(path)
		if err != nil {
			return inserted, fmt.Errorf("cvi: open %s: %w", path, err)
		}
		err = unix.FinitModule(int(f.Fd()), "", 0)
		f.Close()

		// EEXIST means something inserted it between the scan and now, which
		// is a race rather than a failure -- the module is loaded either way,
		// but this process did not do it, so it must not unload it.
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return inserted, fmt.Errorf("cvi: insmod %s: %w", path, err)
		}
		inserted = append(inserted, name)
	}
	return inserted, nil
}

// unloadMediaModules removes the given modules in reverse load order.
//
// Failures are reported but do not stop the walk. A module still in use is the
// expected reason -- another process may hold a device open -- and in that case
// leaving it loaded is correct, not something to force.
func unloadMediaModules(inserted []string) error {
	var firstErr error
	for i := len(inserted) - 1; i >= 0; i-- {
		if err := unix.DeleteModule(inserted[i], 0); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("cvi: rmmod %s: %w", inserted[i], err)
			}
		}
	}
	return firstErr
}
