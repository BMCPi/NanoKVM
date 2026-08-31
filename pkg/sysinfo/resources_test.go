package sysinfo

import (
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

// resources_test.go pins the arithmetic, because every one of these numbers is
// a ratio of two things read from a text file and every one of them has a way
// of coming out plausible-but-wrong: a CPU percentage that counts iowait as
// work, a memory percentage that uses MemFree instead of MemAvailable, a disk
// percentage that disagrees with df because it forgot the root reserve.

const procStatSample = `cpu  1000 20 300 8000 500 10 40 0 0 0
cpu0 1000 20 300 8000 500 10 40 0 0 0
intr 12345
ctxt 67890
`

func TestCPUTimesSplitsBusyFromIdle(t *testing.T) {
	ps, err := parseProcStat(strings.NewReader(procStatSample))
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	busy, total := ps.busy, ps.total

	// idle(8000) + iowait(500) is not work. Everything else is.
	const wantTotal = 1000 + 20 + 300 + 8000 + 500 + 10 + 40
	const wantBusy = wantTotal - 8000 - 500
	if total != wantTotal {
		t.Errorf("total = %d, want %d", total, wantTotal)
	}
	if busy != wantBusy {
		t.Errorf("busy = %d, want %d — iowait is the field this gets wrong", busy, wantBusy)
	}
}

// A kernel built without CONFIG_SCHEDSTATS-era fields still reports the first
// seven; anything shorter is a file we do not understand and must not guess at.
func TestCPUTimesToleratesShortAndRejectsGarbage(t *testing.T) {
	if _, err := parseProcStat(strings.NewReader("cpu 1 2 3 4\n")); err != nil {
		t.Errorf("a four-field cpu line should still parse: %v", err)
	}
	for _, in := range []string{"", "intr 1\n", "cpu\n", "cpu a b c d\n"} {
		if _, err := parseProcStat(strings.NewReader(in)); err == nil {
			t.Errorf("parseProcStat(%q) = nil error, want a refusal", in)
		}
	}
}

const procMemSample = `MemTotal:         246789 kB
MemFree:           12345 kB
MemAvailable:     123456 kB
Buffers:            4567 kB
`

func TestMemoryUsesAvailableNotFree(t *testing.T) {
	total, avail, err := parseMemInfo(strings.NewReader(procMemSample))
	if err != nil {
		t.Fatalf("parseMemInfo: %v", err)
	}
	if total != 246789 {
		t.Errorf("total = %d kB, want 246789", total)
	}
	// MemFree is 12345; picking it up instead of MemAvailable is the classic
	// error and it reports a nearly-full machine that is nothing of the sort.
	if avail != 123456 {
		t.Errorf("available = %d kB, want 123456 (MemAvailable, not MemFree)", avail)
	}
}

// MemAvailable arrived in Linux 3.14. On anything older the honest answer is
// an error, not a percentage derived from MemFree that reads far too high.
func TestMemoryRefusesWithoutMemAvailable(t *testing.T) {
	old := "MemTotal:  246789 kB\nMemFree:    12345 kB\n"
	if _, _, err := parseMemInfo(strings.NewReader(old)); err == nil {
		t.Error("parseMemInfo accepted a meminfo with no MemAvailable")
	}
}

// The percentage is the ratio the operator can act on: how much of the space
// they could actually use is gone. That is df's Use%, which excludes the
// root-reserved blocks from the denominator — not used/total.
func TestDiskPercentMatchesDf(t *testing.T) {
	// 1000 blocks total, 200 free, of which 100 are reserved for root.
	// df: used=800, avail=100, Use% = 800/900 = 88.9%
	got := diskPercent(1000, 200, 100)
	if math.Abs(got-88.888) > 0.01 {
		t.Errorf("diskPercent = %.3f, want ~88.889 (df's Use%%, not used/total = 80)", got)
	}
}

func TestDiskPercentSurvivesAnEmptyFilesystem(t *testing.T) {
	if got := diskPercent(0, 0, 0); got != 0 {
		t.Errorf("diskPercent on a zero-block filesystem = %v, want 0 (not NaN)", got)
	}
}

// The first sample of a rate has no predecessor, so there is no percentage to
// report. Reporting 0 instead would draw a trough that never happened.
func TestCPUPercentNeedsTwoSamples(t *testing.T) {
	var s ResourceSampler

	first, ok := s.cpuPercent(700, 1000)
	if ok {
		t.Errorf("the first reading yielded %v; it has nothing to compare against", first)
	}

	// 100 more ticks of which 50 busy.
	got, ok := s.cpuPercent(750, 1100)
	if !ok {
		t.Fatal("the second reading should produce a percentage")
	}
	if math.Abs(got-50) > 0.001 {
		t.Errorf("cpuPercent = %v, want 50", got)
	}
}

// /proc/stat counters are monotonic; if they ever go backwards the machine
// rebooted underneath us and the interval spans nothing real.
func TestCPUPercentIgnoresACounterReset(t *testing.T) {
	var s ResourceSampler
	s.cpuPercent(700, 1000)

	if _, ok := s.cpuPercent(10, 20); ok {
		t.Error("a backwards counter produced a percentage instead of a gap")
	}
	// ...and the reset becomes the new baseline rather than poisoning the next.
	if got, ok := s.cpuPercent(20, 40); !ok || math.Abs(got-50) > 0.001 {
		t.Errorf("after a reset: got %v ok=%v, want 50 true", got, ok)
	}
}

// A percentage that leaves the 0..100 range makes the fixed-domain chart draw
// outside its own plot area.
func TestCPUPercentStaysInRange(t *testing.T) {
	var s ResourceSampler
	s.cpuPercent(0, 0)
	// Busy grew more than total did — impossible, but clamp rather than trust.
	if got, _ := s.cpuPercent(200, 100); got > 100 {
		t.Errorf("cpuPercent = %v, want it clamped to 100", got)
	}
}

// The ring is the only unbounded-looking thing in this package, and it runs
// forever on a device with 256 MB of RAM.
func TestResourceHistoryIsBounded(t *testing.T) {
	resetResourceHistory(t)

	for i := 0; i < resourceDepth*2; i++ {
		appendResourceSample(validUsage(40), "00:00")
	}
	if n := len(ResourceHistory()); n > resourceDepth {
		t.Errorf("history grew to %d readings, want at most %d", n, resourceDepth)
	}
}

// The first sample has no CPU rate behind it. Recording it would put a zero at
// the left edge of every graph for the next half hour.
func TestFirstUnprimedSampleIsNotRecorded(t *testing.T) {
	resetResourceHistory(t)

	unprimed := Usage{CPURead: true, MemPercent: 40, MemValid: true}
	appendResourceSample(unprimed, "00:00")
	if n := len(historyPoints()); n != 0 {
		t.Fatalf("stored %d points for the un-primed first sample, want 0", n)
	}

	appendResourceSample(validUsage(12), "00:10")
	if got := historyPoints(); len(got) != 1 || got[0].CPU != 12 {
		t.Errorf("after priming: %+v, want one point at 12%%", got)
	}
}

// ...but where /proc/stat cannot be read at all, CPUValid never becomes true.
// Holding out for it would suppress the memory and disk graphs forever.
func TestAMachineWithNoCPUCounterStillGetsAHistory(t *testing.T) {
	resetResourceHistory(t)

	noCPU := Usage{CPURead: false, MemPercent: 55, MemValid: true}
	appendResourceSample(noCPU, "00:00")
	appendResourceSample(noCPU, "00:10")

	got := ResourceHistory()
	if len(got) != 2 {
		t.Fatalf("history = %d points, want 2 — an unreadable /proc/stat must "+
			"not suppress the other two graphs", len(got))
	}
	if got[0].Memory != 55 {
		t.Errorf("memory = %v, want 55", got[0].Memory)
	}
}

// A reading that could not be taken is not a reading of zero. Plotting one
// draws a trough the machine never had.
func TestAnUnreadableSampleCarriesTheLastValueForward(t *testing.T) {
	resetResourceHistory(t)

	appendResourceSample(validUsage(30), "00:00")
	appendResourceSample(validUsage(35), "00:10")
	// Everything fails this tick: /proc unreadable, statfs errored.
	appendResourceSample(Usage{CPURead: true}, "00:20")

	got := ResourceHistory()
	last := got[len(got)-1]
	if last.CPU == 0 || last.Memory == 0 || last.Disk == 0 {
		t.Errorf("a failed sample recorded zeros (%+v); it should carry "+
			"%+v forward", last, got[len(got)-2])
	}
	if last.CPU != 35 {
		t.Errorf("carried CPU = %v, want the previous 35", last.CPU)
	}
}

// Once the ring has wrapped, the oldest stored point is a real reading. An
// earlier version sliced points[1:] on every read to drop the un-primed first
// sample, and went on discarding a good point forever after that sample had
// been evicted.
func TestAFullRingDoesNotKeepDiscardingItsOldestReading(t *testing.T) {
	resetResourceHistory(t)

	for i := 0; i < resourceDepth+10; i++ {
		appendResourceSample(validUsage(float64(i%90)+1), "00:00")
	}
	if n := len(ResourceHistory()); n != resourceDepth {
		t.Errorf("a full ring returned %d readings, want all %d", n, resourceDepth)
	}
}

// ResourceHistory hands out a copy; a caller ranging over it while the sampler
// ticks must not see the slice mutate underneath them.
func TestResourceHistoryHandsOutACopy(t *testing.T) {
	resetResourceHistory(t)
	appendResourceSample(validUsage(7), "00:00")
	appendResourceSample(validUsage(7), "00:10")

	got := ResourceHistory()
	got[0].CPU = 999
	if again := ResourceHistory(); again[0].CPU != 7 {
		t.Errorf("mutating the returned slice changed the history: %v", again[0].CPU)
	}
}

func validUsage(pct float64) Usage {
	return Usage{
		CPUPercent: pct, CPUValid: true, CPURead: true,
		MemPercent: pct, MemValid: true,
		DiskPercent: pct, DiskValid: true,
	}
}

func historyPoints() []ResourcePoint {
	resources.mu.Lock()
	defer resources.mu.Unlock()
	return resources.points.Snapshot()
}

func resetResourceHistory(t *testing.T) {
	t.Helper()
	reset := func() {
		resources.mu.Lock()
		defer resources.mu.Unlock()
		resources.points = utils.NewRing[ResourcePoint](resourceDepth)
		resources.latest = Usage{}
	}
	reset()
	t.Cleanup(reset)
}

// iowait is not monotonic (Documentation/filesystems/proc.rst), and it is
// counted as idle here — so a drop in it shrinks the total by more than it
// shrinks the busy half, and the ratio comes out above one.
func TestCPUPercentRefusesWhenIowaitWentBackwards(t *testing.T) {
	var s ResourceSampler
	// busy=700 total=1000 → idle 300, of which some is iowait.
	s.cpuPercent(700, 1000)

	// Busy grew 50, but total grew only 20 because iowait fell by 30.
	if got, ok := s.cpuPercent(750, 1020); ok {
		t.Errorf("cpuPercent = %v, ok=true; a busy delta larger than the total "+
			"delta is a non-monotonic iowait, not a %v%% interval", got, got)
	}
	// The reading still becomes the baseline, so the next interval is usable.
	if got, ok := s.cpuPercent(760, 1040); !ok || math.Abs(got-50) > 0.001 {
		t.Errorf("next interval: got %v ok=%v, want 50 true", got, ok)
	}
}

// procs_blocked is the one thing in /proc/stat that CPU percent cannot say. A
// BMC stalled on its SD card is idle by every jiffie measure, so the processor
// graph flatlines at exactly the moment an operator needs a signal. The line is
// in a file this collector already opens, below the per-cpu rows.
const procStatBlockedSample = `cpu  1000 20 300 8000 500 10 40 0 0 0
cpu0 1000 20 300 8000 500 10 40 0 0 0
intr 12345
ctxt 67890
btime 1756600000
processes 4321
procs_running 1
procs_blocked 3
softirq 99 1 2 3 4 5 6 7 8 9
`

func TestProcStatReportsBlockedTasks(t *testing.T) {
	ps, err := parseProcStat(strings.NewReader(procStatBlockedSample))
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if !ps.blockedValid {
		t.Fatal("blockedValid = false; procs_blocked is present in the sample")
	}
	if ps.blocked != 3 {
		t.Errorf("blocked = %d, want 3", ps.blocked)
	}
	// Reading past the cpu line must not disturb the jiffie arithmetic.
	if ps.total != 1000+20+300+8000+500+10+40 {
		t.Errorf("total = %d — scanning on for procs_blocked changed the cpu maths", ps.total)
	}
}

// A kernel that does not emit the field must be reported as unknown rather than
// as zero: zero blocked tasks is the healthy reading, and inventing it would
// make an unreadable box look fine.
func TestProcStatDistinguishesAbsentBlockedFromZero(t *testing.T) {
	ps, err := parseProcStat(strings.NewReader(procStatSample)) // no procs_blocked line
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if ps.blockedValid {
		t.Error("blockedValid = true for a /proc/stat with no procs_blocked line")
	}

	zero, err := parseProcStat(strings.NewReader("cpu 1 2 3 4\nprocs_blocked 0\n"))
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if !zero.blockedValid || zero.blocked != 0 {
		t.Errorf("procs_blocked 0 = (%d, valid=%v), want (0, valid=true)", zero.blocked, zero.blockedValid)
	}
}

// The file header claims this collector keeps its per-sample allocation to the
// smallest buffers that will do the job. That claim is only worth making if
// something enforces it: bufio.Scanner's default is a lazily allocated 4 KiB
// per scanner, and dropping the sc.Buffer calls silently takes a sample from
// ~1.6 KB back to ~8.8 KB — to read two files totalling 1661 bytes.
//
// TotalAlloc rather than testing.Benchmark: the allocation *count* is identical
// either way (only the sizes change), and the benchmark harness costs a second
// per parser in a package that otherwise runs in milliseconds.
//
// The budget is deliberately loose. It exists to catch the 4 KiB default coming
// back, not to police a hundred bytes.
func TestParsersDoNotAllocateOversizedBuffers(t *testing.T) {
	const (
		budget = 2048 // bytes/op; the default-buffer regression lands at ~4.3 KB
		runs   = 2000
	)

	allocBytesPerOp := func(fn func()) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range runs {
			fn()
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}

	for _, tc := range []struct {
		name string
		in   string
		run  func(r *strings.Reader)
	}{
		{"procStat", procStatBlockedSample, func(r *strings.Reader) { _, _ = parseProcStat(r) }},
		{"memInfo", procMemSample, func(r *strings.Reader) { _, _, _ = parseMemInfo(r) }},
	} {
		r := strings.NewReader(tc.in)
		got := allocBytesPerOp(func() {
			r.Reset(tc.in)
			tc.run(r)
		})
		if got > budget {
			t.Errorf("%s allocates %d B/op, budget %d — has a scanner lost its sc.Buffer call?",
				tc.name, got, budget)
		}
	}
}
