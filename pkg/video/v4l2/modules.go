package v4l2

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The modules this pipeline needs, by leaf. Everything else -- the vendor
// chain (soph_sys/base/vi/vpss/vc_driver...) and the in-tree V4L2 core
// (videodev, mc, videobuf2-*) -- arrives as recorded dependencies from
// modules.dep, in correct order, which is why this list is three entries and
// not sixteen:
//
//   - soph_v4l2 links against nearly the whole stack, so depmod pulls it in.
//   - soph_mipi_rx (the CSI receiver) and soph_snsr_i2c are driven through
//     the base callback table, not by symbol imports, so nothing depends on
//     them and they must be named explicitly.
//
// Nothing else on the system loads any of this: there is no modules-load.d
// entry, no udev rule, and the image ships no kmod -- raw finit_module it is.
// The names are the stock firmware's soph_* family and must match the built
// .ko names exactly; an old binary against a renamed image finds nothing.
var leafModules = []string{
	"soph_snsr_i2c",
	"soph_mipi_rx",
	"soph_v4l2",
}

var moduleDirs = []string{"/lib/modules", "/usr/lib/modules"}

// depGraph is modules.dep: module path -> its dependency paths, both
// relative to the modules directory for the running kernel.
type depGraph struct {
	base string
	deps map[string][]string
	// byName resolves a bare module name (normalised) to its path.
	byName map[string]string
}

func normaliseModule(name string) string {
	return strings.ReplaceAll(strings.TrimSuffix(name, ".ko"), "-", "_")
}

// loadDepGraph reads modules.dep for the running kernel. depmod runs at
// image build, so the file is always present and already dependency-sorted
// per line: "path: dep1 dep2 ..." with deps listed in load order.
func loadDepGraph() (*depGraph, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return nil, fmt.Errorf("v4l2: uname: %w", err)
	}
	release := unix.ByteSliceToString(uts.Release[:])

	var lastErr error
	for _, dir := range moduleDirs {
		g, err := parseDepGraph(filepath.Join(dir, release))
		if err != nil {
			lastErr = err
			continue
		}
		return g, nil
	}
	return nil, fmt.Errorf("v4l2: no modules.dep for kernel %s under %v: %w",
		release, moduleDirs, lastErr)
}

func parseDepGraph(base string) (*depGraph, error) {
	f, err := os.Open(filepath.Join(base, "modules.dep"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	g := &depGraph{
		base:   base,
		deps:   make(map[string][]string),
		byName: make(map[string]string),
	}
	sc := bufio.NewScanner(f)
	// A line can exceed bufio's default 64KiB only in pathological trees;
	// 1MiB is beyond anything depmod emits for this image.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		path, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		g.deps[path] = strings.Fields(rest)
		g.byName[normaliseModule(filepath.Base(path))] = path
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("v4l2: read modules.dep: %w", err)
	}
	return g, nil
}

// loadedModules reports what is currently inserted. /proc/modules names them
// with underscores regardless of dashes in the filename, hence the
// normalisation.
func loadedModules() (map[string]bool, error) {
	f, err := os.Open("/proc/modules")
	if err != nil {
		return nil, fmt.Errorf("v4l2: read /proc/modules: %w", err)
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

// insert loads one module file if it is not already present, recording it in
// inserted when this process did the loading.
func (g *depGraph) insert(path string, present map[string]bool,
	inserted *[]string,
) error {
	name := normaliseModule(filepath.Base(path))
	if present[name] {
		return nil
	}

	f, err := os.Open(filepath.Join(g.base, path))
	if err != nil {
		return fmt.Errorf("v4l2: open %s: %w", path, err)
	}
	err = unix.FinitModule(int(f.Fd()), "", 0)
	f.Close()

	// EEXIST means something inserted it between the scan and now: a race,
	// not a failure -- but this process did not do it and must not unload it.
	if errors.Is(err, unix.EEXIST) {
		present[name] = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("v4l2: insmod %s: %w", path, err)
	}
	present[name] = true
	*inserted = append(*inserted, name)
	return nil
}

// loadPipelineModules inserts the capture stack, dependencies first, and
// returns the modules this process actually inserted, in load order.
func loadPipelineModules() ([]string, error) {
	g, err := loadDepGraph()
	if err != nil {
		return nil, err
	}
	present, err := loadedModules()
	if err != nil {
		return nil, err
	}

	var inserted []string
	for _, leaf := range leafModules {
		path, ok := g.byName[normaliseModule(leaf)]
		if !ok {
			return inserted, fmt.Errorf("v4l2: %s not in modules.dep", leaf)
		}
		// modules.dep lists a module's full transitive dependency set,
		// deepest last -- so walking it back to front is load order.
		deps := g.deps[path]
		for i := len(deps) - 1; i >= 0; i-- {
			if err := g.insert(deps[i], present, &inserted); err != nil {
				return inserted, err
			}
		}
		if err := g.insert(path, present, &inserted); err != nil {
			return inserted, err
		}
	}
	return inserted, nil
}

// unloadPipelineModules removes the given modules in reverse load order.
// Failures are reported but do not stop the walk: a module still in use is
// the expected reason, and leaving it loaded is correct, not something to
// force.
func unloadPipelineModules(inserted []string) error {
	var firstErr error
	for i := len(inserted) - 1; i >= 0; i-- {
		if err := unix.DeleteModule(inserted[i], 0); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("v4l2: rmmod %s: %w", inserted[i], err)
			}
		}
	}
	return firstErr
}
