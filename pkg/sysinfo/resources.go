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
// deliberate: the target is a 1 GHz single-core SG2002 whose 256 MB of DRAM the
// device tree carves down to 123 MB of usable RAM (the rest is the ION
// reservation the capture pipeline needs), so the collector may not fork, must
// keep its per-sample allocation to the smallest buffers that will do the job,
// and may not pull in a dependency that walks all of /proc to answer three
// questions.
//
// A sample is ~7 syscalls over 1661 bytes of procfs on a 10 s tick — around
// 10^-5 of one core. There is no performance problem here to solve, and that is
// the point: the austerity is what keeps it that way.

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

	// Scanner buffer sizing. bufio.Scanner's default is a lazily allocated
	// 4 KiB, which is most of what a sample allocates — to read a 441-byte
	// /proc/stat and a 1220-byte /proc/meminfo. 512 bytes clears the longest
	// line either file has (the softirq row), and the max keeps the default
	// ceiling as a safety net if a future kernel emits something longer.
	scanBufSize = 512
	scanBufMax  = 64 * 1024
)

// Usage is one reading of the BMC's own resource utilisation. Percentages are
// 0..100. Each subsystem carries its own validity flag because they fail
// independently — a missing data volume on a dev workstation should not blank
// the processor graph.
type Usage struct {
	CPUPercent float64
	// CPUValid is true once a rate could be computed, which needs two
	// readings. CPURead distinguishes the two reasons it might be false: the
	// sampler has only just started (read, not yet valid) versus /proc/stat
	// is not readable at all. The history needs to tell those apart — the
	// first is worth waiting one tick for, the second never resolves.
	CPUValid bool
	CPURead  bool

	MemPercent float64
	MemUsedMB  uint64
	MemTotalMB uint64
	MemValid   bool

	DiskPercent float64
	DiskUsedMB  uint64
	DiskTotalMB uint64
	DiskValid   bool

	// ProcsBlocked is /proc/stat's procs_blocked: tasks in uninterruptible
	// sleep at the moment of the read. It is here because CPU percent cannot
	// say it — a box stalled on the SD card is *idle*, so the processor graph
	// goes quiet at exactly the moment something is wrong. It costs nothing:
	// the line is in a file this collector already opens.
	//
	// It is a depth, not a percentage, and small numbers are normal. What an
	// operator acts on is it staying above zero across several samples.
	ProcsBlocked      uint64
	ProcsBlockedValid bool
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

	if ps, err := readProcStat(); err == nil {
		u.CPURead = true
		u.CPUPercent, u.CPUValid = s.cpuPercent(ps.busy, ps.total)
		u.ProcsBlocked, u.ProcsBlockedValid = ps.blocked, ps.blockedValid
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

	deltaAll, deltaBusy := all-prevAll, busy-prevBusy
	if deltaAll == 0 {
		return 0, false
	}
	// iowait is not monotonic — Documentation/filesystems/proc.rst says so
	// outright — and it is counted as idle here, so a drop in it shrinks the
	// total by more than it shrinks the busy half. That yields a ratio above
	// one out of an interval where nothing unusual happened. Reporting no
	// reading leaves a flat segment; clamping would paint a 100% spike the
	// machine never had.
	if deltaBusy > deltaAll {
		return 0, false
	}
	return clampPercent(float64(deltaBusy) / float64(deltaAll) * 100), true
}

// procStat is one read of /proc/stat.
type procStat struct {
	// busy and total are jiffies from the aggregate "cpu" line.
	busy, total uint64

	// blocked is procs_blocked: how many tasks are in uninterruptible sleep
	// right now, which on this appliance means waiting on the SD card. Unlike
	// the jiffie counters it is an instantaneous depth, not a cumulative
	// total, so it is reported as-is rather than differenced.
	//
	// blockedValid is false on a kernel that does not emit the field. It has
	// been there since 2.6, so absence means procfs is not what we think it
	// is — the same standard parseMemInfo holds MemAvailable to.
	blocked      uint64
	blockedValid bool
}

// readProcStat reads /proc/stat.
func readProcStat() (procStat, error) {
	f, err := os.Open(procStatPath)
	if err != nil {
		return procStat{}, err
	}
	defer f.Close()
	return parseProcStat(f)
}

// parseProcStat splits the aggregate "cpu" line into busy and total jiffies,
// and picks up procs_blocked on the way past.
//
// The cpu fields, in order, are user nice system idle iowait irq softirq steal
// guest guest_nice. Idle time is idle+iowait: a core blocked on I/O is not
// doing work, and counting iowait as busy is what makes a quiet BMC that is
// writing an ISO look pegged. guest and guest_nice are excluded from total
// entirely: the kernel already folds guest time into user and guest_nice into
// nice (man 5 proc), so summing every field would double-count it.
//
// procs_blocked sits below the per-cpu lines, so this cannot stop at the
// aggregate line the way it used to. The whole file is 441 bytes on this
// board; scanning the rest of it costs nothing worth measuring.
func parseProcStat(r io.Reader) (procStat, error) {
	const (
		idleField   = 3
		iowaitField = 4
		guestField  = 8
		// guestNiceField is guest_nice, the last of the ten documented cpu
		// fields; a longer line (a future kernel field) still just adds to
		// total below, same as it always has.
		guestNiceField = 9
		minFields      = 4 // user nice system idle — the shortest line worth trusting
	)

	var (
		out     procStat
		haveCPU bool
	)

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, scanBufSize), scanBufMax)
	for sc.Scan() {
		line := sc.Text()

		if !haveCPU && (strings.HasPrefix(line, "cpu ") || strings.HasPrefix(line, "cpu\t")) {
			fields := strings.Fields(line)[1:]
			if len(fields) < minFields {
				return procStat{}, fmt.Errorf("%s: cpu line has %d fields, want at least %d",
					procStatPath, len(fields), minFields)
			}

			var idle uint64
			for i, f := range fields {
				v, convErr := strconv.ParseUint(f, 10, 64)
				if convErr != nil {
					return procStat{}, fmt.Errorf("%s: cpu field %d (%q): %w",
						procStatPath, i, f, convErr)
				}
				// guest and guest_nice are already folded into user/nice by
				// the kernel (man 5 proc); adding them to total again would
				// double-count guest time.
				if i == guestField || i == guestNiceField {
					continue
				}
				out.total += v
				if i == idleField || i == iowaitField {
					idle += v
				}
			}
			out.busy = out.total - idle
			haveCPU = true
			continue
		}

		if rest, ok := strings.CutPrefix(line, "procs_blocked"); ok {
			v, convErr := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
			if convErr != nil {
				// Not fatal: the CPU numbers are the reason this file is read,
				// and they are already in hand. Leave blockedValid false so the
				// gauge reports nothing rather than a fabricated zero.
				continue
			}
			out.blocked, out.blockedValid = v, true
		}
	}
	if err := sc.Err(); err != nil {
		return procStat{}, err
	}
	if !haveCPU {
		return procStat{}, errors.New(procStatPath + ": no aggregate cpu line")
	}
	return out, nil
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
	sc.Buffer(make([]byte, 0, scanBufSize), scanBufMax)
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
