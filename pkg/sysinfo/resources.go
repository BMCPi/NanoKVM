package sysinfo

// resources.go reads what the BMC itself is using: processor, memory and the
// writable data volume.
//
// These are the BMC's numbers, not the managed host's. The host is a Raspberry
// Pi running UEFI firmware, and firmware does not report live utilisation over
// the Redfish host interface — there is no OS agent on the other side to ask.
// So the drawer's graphs describe the appliance an operator is talking to,
// which is the machine that can actually run out of room mid-upload.
//
// Everything here is parsed out of procfs and one statfs call. That is
// deliberate: the target is a 1 GHz single-core SG2002 with ~256 MB of RAM, so
// the collector may not fork, may not allocate per-sample buffers of any size,
// and may not pull in a dependency that walks all of /proc to answer three
// questions.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	procStatPath    = "/proc/stat"
	procMemInfoPath = "/proc/meminfo"

	// dataVolumePath is the writable partition the initramfs mounts (see
	// pkg/config/default.go): ISOs, capsules, logs and the config all land
	// there, and it is the only filesystem on the device that an operator can
	// fill. The root is squashfs and /tmp is a small RAM overlay, so neither
	// tells them anything they can act on.
	dataVolumePath = "/var/lib/nanokvm"
)

// Usage is one reading of the BMC's own resource utilisation. Percentages are
// 0..100. Each subsystem carries its own validity flag because they fail
// independently — a missing data volume on a dev workstation should not blank
// the processor graph.
type Usage struct {
	CPUPercent float64
	CPUValid   bool

	MemPercent float64
	MemUsedMB  uint64
	MemTotalMB uint64
	MemValid   bool

	DiskPercent float64
	DiskUsedMB  uint64
	DiskTotalMB uint64
	DiskValid   bool
}

// ResourceSampler turns the cumulative counters in /proc/stat into a rate. It
// holds the previous reading, so it must be reused across samples rather than
// constructed per call — a fresh sampler can never report a CPU percentage.
//
// The zero value is ready to use. Safe for concurrent use: the HTTP fragment
// and the metrics callback both read through the same instance.
type ResourceSampler struct {
	mu       sync.Mutex
	prevBusy uint64
	prevAll  uint64
	primed   bool
}

// Sample reads all three subsystems. Failures are reported per-subsystem
// through the Valid flags rather than as an error: a BMC that cannot stat its
// data volume can still show its processor load, and the graphs are a
// convenience that must never take the drawer down with them.
func (s *ResourceSampler) Sample() Usage {
	var u Usage

	if busy, all, err := readCPUTimes(); err == nil {
		u.CPUPercent, u.CPUValid = s.cpuPercent(busy, all)
	}

	if totalKB, availKB, err := readMemInfo(); err == nil && totalKB > 0 {
		usedKB := totalKB - min(availKB, totalKB)
		u.MemPercent = clampPercent(float64(usedKB) / float64(totalKB) * 100)
		u.MemUsedMB = usedKB / 1024
		u.MemTotalMB = totalKB / 1024
		u.MemValid = true
	}

	if d, err := readDisk(dataVolumePath); err == nil {
		u = d.applyTo(u)
	}

	return u
}

// cpuPercent converts two cumulative readings into the busy fraction of the
// interval between them. The second return is false when there is no interval
// to report on: the first ever sample, a counter reset, or two reads so close
// together that no tick elapsed.
func (s *ResourceSampler) cpuPercent(busy, all uint64) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prevBusy, prevAll, primed := s.prevBusy, s.prevAll, s.primed
	s.prevBusy, s.prevAll, s.primed = busy, all, true

	// A backwards counter means the kernel's counters restarted underneath us.
	// The interval spans a reboot and describes nothing; this reading becomes
	// the new baseline instead.
	if !primed || busy < prevBusy || all < prevAll {
		return 0, false
	}

	deltaAll := all - prevAll
	if deltaAll == 0 {
		return 0, false
	}
	return clampPercent(float64(busy-prevBusy) / float64(deltaAll) * 100), true
}

// readCPUTimes reads the aggregate cpu line from /proc/stat.
func readCPUTimes() (busy, total uint64, err error) {
	f, err := os.Open(procStatPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseCPUTimes(f)
}

// parseCPUTimes splits the aggregate "cpu" line into busy and total jiffies.
//
// The fields, in order, are user nice system idle iowait irq softirq steal
// guest guest_nice. Idle time is idle+iowait: a core blocked on I/O is not
// doing work, and counting iowait as busy is what makes a quiet BMC that is
// writing an ISO look pegged.
func parseCPUTimes(r io.Reader) (busy, total uint64, err error) {
	const (
		idleField   = 3
		iowaitField = 4
		minFields   = 4 // user nice system idle — the shortest line worth trusting
	)

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") && !strings.HasPrefix(line, "cpu\t") {
			continue
		}

		fields := strings.Fields(line)[1:]
		if len(fields) < minFields {
			return 0, 0, fmt.Errorf("%s: cpu line has %d fields, want at least %d",
				procStatPath, len(fields), minFields)
		}

		var idle uint64
		for i, f := range fields {
			v, convErr := strconv.ParseUint(f, 10, 64)
			if convErr != nil {
				return 0, 0, fmt.Errorf("%s: cpu field %d (%q): %w", procStatPath, i, f, convErr)
			}
			total += v
			if i == idleField || i == iowaitField {
				idle += v
			}
		}
		return total - idle, total, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, errors.New(procStatPath + ": no aggregate cpu line")
}

// readMemInfo reads MemTotal and MemAvailable from /proc/meminfo, in kB.
func readMemInfo() (totalKB, availKB uint64, err error) {
	f, err := os.Open(procMemInfoPath)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseMemInfo(f)
}

// parseMemInfo pulls MemTotal and MemAvailable out of /proc/meminfo.
//
// MemAvailable rather than MemFree, and an error rather than a fallback when it
// is absent. MemFree excludes the page cache, so on a machine that has just
// streamed an ISO it reports almost nothing free while almost all of it is
// reclaimable — a graph pinned at 95% that means nothing is worse than no
// graph. MemAvailable has been in the kernel since 3.14; the device is far
// newer, so its absence means procfs is not what we think it is.
func parseMemInfo(r io.Reader) (totalKB, availKB uint64, err error) {
	var haveTotal, haveAvail bool

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB, haveTotal = parseKB(rest), true
		case "MemAvailable":
			availKB, haveAvail = parseKB(rest), true
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

// parseKB reads the "  246789 kB" tail of a meminfo line. An unparseable value
// reads as zero, which the caller's total>0 guard turns into "no reading".
func parseKB(s string) uint64 {
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

// diskUsage is the statfs result in the shape Usage wants. Sizes are unsigned
// throughout: they are byte counts, they can never be negative, and keeping
// them unsigned is what lets statfs's own uint64 fields through without a
// conversion whose overflow behaviour someone would have to reason about.
type diskUsage struct {
	percent float64
	usedMB  uint64
	totalMB uint64
}

func (d diskUsage) applyTo(u Usage) Usage {
	u.DiskPercent, u.DiskUsedMB, u.DiskTotalMB, u.DiskValid = d.percent, d.usedMB, d.totalMB, true
	return u
}

// readDisk stats the given filesystem.
func readDisk(path string) (diskUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return diskUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	// Bsize is the one signed field in Statfs_t. A negative block size is not
	// a thing any filesystem reports, and every target this builds for
	// (riscv64, arm64, amd64) is 64-bit, so the block counts beside it are
	// already uint64 and need no widening.
	bsize := uint64(st.Bsize) //nolint:gosec // block size is never negative
	blocks, bfree, bavail := st.Blocks, st.Bfree, st.Bavail

	const bytesPerMB = 1024 * 1024
	used := (blocks - min(bfree, blocks)) * bsize
	return diskUsage{
		percent: diskPercent(blocks, bfree, bavail),
		usedMB:  used / bytesPerMB,
		totalMB: blocks * bsize / bytesPerMB,
	}, nil
}

// diskPercent is df's Use%: the share of the space actually available to this
// user that is gone. The reserved blocks (bfree - bavail, held back for root)
// are excluded from both halves, because an operator cannot put an ISO there
// and a percentage they cannot act on is noise. used/total instead of this
// reports a comfortable number on a volume that is already refusing writes.
func diskPercent(blocks, bfree, bavail uint64) float64 {
	used := blocks - min(bfree, blocks)
	usable := used + bavail
	if usable == 0 {
		return 0
	}
	return clampPercent(float64(used) / float64(usable) * 100)
}

// clampPercent keeps a ratio inside the fixed 0..100 domain the charts draw
// against. Nothing here should ever leave it, but a graph that paints outside
// its own plot area is a worse way to find that out than a flat line at 100.
func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
