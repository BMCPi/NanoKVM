package utils

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
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
		log.Debug("GOMEMLIMIT already set via environment; leaving as-is")
		return
	}

	if IsGoMemLimitExist() {
		if limit, err := GetGoMemLimit(); err == nil {
			debug.SetMemoryLimit(limit * 1024 * 1024)
			log.Infof("set GOMEMLIMIT to %d MB (from %s)", limit, GoMemLimitFile)
			return
		}
	}

	limit := defaultMemLimitMB()
	debug.SetMemoryLimit(limit * 1024 * 1024)
	log.Infof("set GOMEMLIMIT to %d MB (auto, %d%% of system RAM)", limit, memLimitPercent)
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

	log.Debugf("set GOMEMLIMIT to %d MB", limit)

	data := []byte(fmt.Sprintf("%d", limit))
	err := os.WriteFile(GoMemLimitFile, data, 0o644)
	if err != nil {
		log.Errorf("failed to write GOMEMLIMIT: %s", err)
		return err
	}

	return nil
}

func GetGoMemLimit() (int64, error) {
	data, err := os.ReadFile(GoMemLimitFile)
	if err != nil {
		log.Errorf("failed to read GOMEMLIMIT: %s", err)
		return 0, err
	}

	content := strings.TrimSpace(string(data))
	limit, err := strconv.ParseInt(content, 10, 64)
	if err != nil {
		log.Errorf("failed to parse GOMEMLIMIT: %s", err)
		return 0, err
	}

	return limit, nil
}

func DelGoMemLimit() error {
	debug.SetMemoryLimit(1024 * 1024 * 1024)

	err := os.Remove(GoMemLimitFile)
	if err != nil {
		log.Errorf("failed to delete GOMEMLIMIT: %s", err)
		return err
	}

	return nil
}

func IsGoMemLimitExist() bool {
	_, err := os.Stat(GoMemLimitFile)
	return err == nil
}
