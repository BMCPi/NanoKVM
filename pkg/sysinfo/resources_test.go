package sysinfo

import (
	"math"
	"strings"
	"testing"
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
	busy, total, err := parseCPUTimes(strings.NewReader(procStatSample))
	if err != nil {
		t.Fatalf("parseCPUTimes: %v", err)
	}

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
	if _, _, err := parseCPUTimes(strings.NewReader("cpu 1 2 3 4\n")); err != nil {
		t.Errorf("a four-field cpu line should still parse: %v", err)
	}
	for _, in := range []string{"", "intr 1\n", "cpu\n", "cpu a b c d\n"} {
		if _, _, err := parseCPUTimes(strings.NewReader(in)); err == nil {
			t.Errorf("parseCPUTimes(%q) = nil error, want a refusal", in)
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
func TestResourceHistoryIsBoundedAndDropsTheFirstPoint(t *testing.T) {
	resetResourceHistory(t)

	// One point is not a history: the sole reading's CPU is always zero.
	pushResourcePoint(ResourcePoint{At: "00:00", CPU: 0, Memory: 40})
	if got := ResourceHistory(); got != nil {
		t.Errorf("history with one reading = %v, want nil", got)
	}

	pushResourcePoint(ResourcePoint{At: "00:01", CPU: 12, Memory: 41})
	got := ResourceHistory()
	if len(got) != 1 || got[0].At != "00:01" {
		t.Fatalf("history = %+v, want just the second reading", got)
	}

	for i := 0; i < resourceDepth*2; i++ {
		pushResourcePoint(ResourcePoint{At: "01:00", CPU: 1})
	}
	if n := len(ResourceHistory()); n > resourceDepth {
		t.Errorf("history grew to %d readings, want at most %d", n, resourceDepth)
	}
}

// ResourceHistory hands out a copy; a caller ranging over it while the sampler
// ticks must not see the slice mutate underneath them.
func TestResourceHistoryHandsOutACopy(t *testing.T) {
	resetResourceHistory(t)
	pushResourcePoint(ResourcePoint{At: "00:00"})
	pushResourcePoint(ResourcePoint{At: "00:01", CPU: 7})

	got := ResourceHistory()
	got[0].CPU = 999
	if again := ResourceHistory(); again[0].CPU != 7 {
		t.Errorf("mutating the returned slice changed the history: %v", again[0].CPU)
	}
}

func resetResourceHistory(t *testing.T) {
	t.Helper()
	resources.mu.Lock()
	defer resources.mu.Unlock()
	resources.points = nil
	resources.latest = Usage{}
	t.Cleanup(func() {
		resources.mu.Lock()
		defer resources.mu.Unlock()
		resources.points = nil
		resources.latest = Usage{}
	})
}

// pushResourcePoint goes through the production ring rather than reproducing
// it, so the bound above is actually the bound recordResourceSample enforces.
func pushResourcePoint(p ResourcePoint) {
	appendResourcePoint(Usage{CPUPercent: p.CPU, MemPercent: p.Memory, DiskPercent: p.Disk}, p)
}
