package memlimit

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const GoMemLimitFile = "/etc/kvm/GOMEMLIMIT"

const procMemInfoPath = "/proc/meminfo"

// memInfoScanBufSize/memInfoScanBufMax size bufio.Scanner's buffer for
// /proc/meminfo. The default is a lazily allocated 4 KiB; 512 bytes clears
// the longest line the file has, and the max keeps the default ceiling as a
// safety net if a future kernel emits something longer.
const (
	memInfoScanBufSize = 512
	memInfoScanBufMax  = 64 * 1024
)

// Auto memory-limit bounds. On a memory-constrained device, running with Go's
// default (no limit) lets the heap grow to ~2x the live set with no GC
// back-pressure, so a transient spike or a leak can OOM the box. When no
// explicit limit is configured we derive a soft limit from total system RAM.
const (
	memLimitPercent = 70  // fraction of MemTotal to use as the soft limit
	minMemLimitMB   = 64  // floor so tiny/unreadable RAM figures stay usable
	maxMemLimitMB   = 512 // ceiling; beyond this the limit adds little value
)

// InitGoMemLimit applies a soft heap limit (GOMEMLIMIT) so the GC pushes back
// before the process exhausts memory. Precedence:
//  1. an explicit GOMEMLIMIT env var (already applied by the runtime) is left
//     untouched;
//  2. otherwise the value in GoMemLimitFile, if present, is used;
//  3. otherwise a limit derived from total system RAM is applied.
func InitGoMemLimit(log *slog.Logger) {
	// debug.SetMemoryLimit(-1) reports the current limit without changing it.
	// math.MaxInt64 means "no limit set" — anything else came from the
	// GOMEMLIMIT env var, which we respect.
	if debug.SetMemoryLimit(-1) != math.MaxInt64 {
		log.Debug("GOMEMLIMIT already set via environment; leaving as-is")
		return
	}

	if IsGoMemLimitExist() {
		if limit, err := GetGoMemLimit(log); err == nil {
			debug.SetMemoryLimit(limit * 1024 * 1024)
			log.Info("set GOMEMLIMIT from file", slog.Int64("limitMB", limit), slog.String("path", GoMemLimitFile))
			return
		}
	}

	limit := defaultMemLimitMB()
	debug.SetMemoryLimit(limit * 1024 * 1024)
	log.Info("set GOMEMLIMIT automatically", slog.Int64("limitMB", limit), slog.Int("percentOfRAM", memLimitPercent))
}

// defaultMemLimitMB derives a soft limit from MemTotal, clamped to sane bounds.
func defaultMemLimitMB() int64 {
	total := readMemTotalMB()
	if total <= 0 {
		return minMemLimitMB
	}
	limit := total * memLimitPercent / 100
	if limit < minMemLimitMB {
		return minMemLimitMB
	}
	if limit > maxMemLimitMB {
		return maxMemLimitMB
	}
	return limit
}

// readMemTotalMB returns total system memory in MB from /proc/meminfo, or 0.
func readMemTotalMB() int64 {
	f, err := os.Open(procMemInfoPath)
	if err != nil {
		return 0
	}
	defer f.Close()

	totalKB, _, err := ParseMemInfo(f)
	if err != nil {
		return 0
	}
	//nolint:gosec // a kB memory figure from /proc/meminfo fits comfortably in an int64
	return int64(totalKB / 1024)
}

// ParseMemInfo pulls MemTotal and MemAvailable out of r, in /proc/meminfo
// format, returning kB values for both.
//
// MemAvailable rather than MemFree, and an error rather than a fallback when
// it is absent. MemFree excludes the page cache, so on a machine that has
// just streamed a large file it reports almost nothing free while almost all
// of it is reclaimable — a reading pinned near 100% that means nothing is
// worse than no reading. MemAvailable has been in the kernel since 3.14,
// well before any device this ships on, so its absence means procfs is not
// what this code thinks it is.
func ParseMemInfo(r io.Reader) (totalKB, availKB uint64, err error) {
	var haveTotal, haveAvail bool

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, memInfoScanBufSize), memInfoScanBufMax)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB, haveTotal = parseMemInfoKB(rest), true
		case "MemAvailable":
			availKB, haveAvail = parseMemInfoKB(rest), true
		default:
			continue
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("%s: want MemTotal and MemAvailable, got total=%v available=%v",
			procMemInfoPath, haveTotal, haveAvail)
	}
	return totalKB, availKB, nil
}

// parseMemInfoKB reads the "  246789 kB" tail of a meminfo line. An
// unparseable value reads as zero, which readMemTotalMB's total>0-equivalent
// guards turn into "no reading" rather than a fabricated figure.
func parseMemInfoKB(s string) uint64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func SetGoMemLimit(limit int64, log *slog.Logger) error {
	memoryLimit := max(limit, 50)
	debug.SetMemoryLimit(memoryLimit * 1024 * 1024)

	log.Debug("set GOMEMLIMIT", slog.Int64("limitMB", limit))

	data := []byte(fmt.Sprintf("%d", limit))
	// 0600: this file is written and read only by this (root) process — no
	// init script, no other component on the image references the path — so
	// there is no reader whose access the narrower mode would take away.
	err := os.WriteFile(GoMemLimitFile, data, 0o600)
	if err != nil {
		log.Error("failed to write GOMEMLIMIT", slog.Any("err", err))
		return err
	}

	return nil
}

func GetGoMemLimit(log *slog.Logger) (int64, error) {
	data, err := os.ReadFile(GoMemLimitFile)
	if err != nil {
		log.Error("failed to read GOMEMLIMIT", slog.Any("err", err))
		return 0, err
	}

	content := strings.TrimSpace(string(data))
	limit, err := strconv.ParseInt(content, 10, 64)
	if err != nil {
		log.Error("failed to parse GOMEMLIMIT", slog.Any("err", err))
		return 0, err
	}

	return limit, nil
}

func DelGoMemLimit(log *slog.Logger) error {
	debug.SetMemoryLimit(1024 * 1024 * 1024)

	err := os.Remove(GoMemLimitFile)
	if err != nil {
		log.Error("failed to delete GOMEMLIMIT", slog.Any("err", err))
		return err
	}

	return nil
}

func IsGoMemLimitExist() bool {
	_, err := os.Stat(GoMemLimitFile)
	return err == nil
}
