package utils

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const GoMemLimitFile = "/etc/kvm/GOMEMLIMIT"

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
func InitGoMemLimit() {
	// debug.SetMemoryLimit(-1) reports the current limit without changing it.
	// math.MaxInt64 means "no limit set" — anything else came from the
	// GOMEMLIMIT env var, which we respect.
	if debug.SetMemoryLimit(-1) != math.MaxInt64 {
		slog.Debug("GOMEMLIMIT already set via environment; leaving as-is")
		return
	}

	if IsGoMemLimitExist() {
		if limit, err := GetGoMemLimit(); err == nil {
			debug.SetMemoryLimit(limit * 1024 * 1024)
			slog.Info("set GOMEMLIMIT from file", slog.Int64("limitMB", limit), slog.String("path", GoMemLimitFile))
			return
		}
	}

	limit := defaultMemLimitMB()
	debug.SetMemoryLimit(limit * 1024 * 1024)
	slog.Info("set GOMEMLIMIT automatically", slog.Int64("limitMB", limit), slog.Int("percentOfRAM", memLimitPercent))
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
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // ["MemTotal:", "<kB>", "kB"]
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

func SetGoMemLimit(limit int64) error {
	memoryLimit := max(limit, 50)
	debug.SetMemoryLimit(memoryLimit * 1024 * 1024)

	slog.Debug("set GOMEMLIMIT", slog.Int64("limitMB", limit))

	data := []byte(fmt.Sprintf("%d", limit))
	// 0600: this file is written and read only by this (root) process — no
	// init script, no other component on the image references the path — so
	// there is no reader whose access the narrower mode would take away.
	err := os.WriteFile(GoMemLimitFile, data, 0o600)
	if err != nil {
		slog.Error("failed to write GOMEMLIMIT", slog.Any("err", err))
		return err
	}

	return nil
}

func GetGoMemLimit() (int64, error) {
	data, err := os.ReadFile(GoMemLimitFile)
	if err != nil {
		slog.Error("failed to read GOMEMLIMIT", slog.Any("err", err))
		return 0, err
	}

	content := strings.TrimSpace(string(data))
	limit, err := strconv.ParseInt(content, 10, 64)
	if err != nil {
		slog.Error("failed to parse GOMEMLIMIT", slog.Any("err", err))
		return 0, err
	}

	return limit, nil
}

func DelGoMemLimit() error {
	debug.SetMemoryLimit(1024 * 1024 * 1024)

	err := os.Remove(GoMemLimitFile)
	if err != nil {
		slog.Error("failed to delete GOMEMLIMIT", slog.Any("err", err))
		return err
	}

	return nil
}

func IsGoMemLimitExist() bool {
	_, err := os.Stat(GoMemLimitFile)
	return err == nil
}
