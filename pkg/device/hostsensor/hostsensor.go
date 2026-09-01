// Package hostsensor is the board-agnostic seam between whatever telemetry a
// managed host offers about itself and the consumers that report it: the OTel
// gauges (pkg/platform/telemetry), the IPMI sensor HAL (pkg/protocol/ipmi),
// and the Server Overview's Host Sensors card (ui/fragments/overview.go).
//
// Exactly one Source may be registered per process, by main during startup
// once the board's profile is known. pkg/device/bmcsensor — the Raspberry Pi
// OP-TEE/I2C push record — is the first and, today, only implementation. A
// board with no host-telemetry channel (the NUC target this seam was built
// for) simply never calls Register: Get reports none, and every consumer here
// renders or reports that absence honestly rather than fabricating a sensor.
package hostsensor

import (
	"sync/atomic"
	"time"
)

// Condition is one live power-health state a Source can report: the
// firmware's own words for why it is limiting itself, current rather than
// latched-since-boot (a consumer that wants "has this ever happened" reads
// its own board-specific record for that; this seam only carries what is
// true right now). The names double as the Redfish Oem property names for
// the same states, so the two surfaces cannot drift apart.
type Condition string

const (
	ConditionUnderVoltage  Condition = "UnderVoltage"
	ConditionThrottled     Condition = "Throttled"
	ConditionFreqCapped    Condition = "FrequencyCapped"
	ConditionSoftTempLimit Condition = "SoftTempLimit"
)

// Reading is one sample of what the managed host reports about itself,
// independent of the wire format any particular Source reads it from. Its
// field set is exactly what today's consumers use — pkg/device/bmcsensor's
// own Reading/Record carries more (sequence numbers, raw status bits, the
// latched-since-boot throttle halves) that nothing outside that package
// needs.
type Reading struct {
	// At is when this reading's sequence was first observed, in BMC wall
	// time. Zero on a Reading returned from a Source's history/trend
	// extension, where only the plotted values below are meaningful.
	At time.Time
	// Stale reports that the host has stopped pushing fresh samples; the
	// values below are the last ones observed rather than current. A
	// consumer must not report them as live.
	Stale bool

	// TempC is the host SoC die temperature. Meaningful only when
	// TempValid: a Source can carry a previous value forward rather than a
	// fresh read, and that must not be plotted as one.
	TempC     float64
	TempValid bool

	// Fan* describe the host's active cooler. FanValid is false where the
	// host reports no fan block at all (as opposed to one reporting zero).
	FanDutyPct  float64
	FanLevel    uint8
	FanMaxLevel uint8
	// FanRPM is the measured tachometer speed; meaningful only when
	// FanRPMValid, since a zero there is the host saying it has no tach
	// capture, not a stalled fan.
	FanRPM      uint16
	FanValid    bool
	FanRPMValid bool

	// ThrottleValid reports whether the host sent a power-health reading at
	// all; Conditions is only meaningful when this is true — a host whose
	// firmware predates the word reports no conditions too, and the two must
	// not be conflated into "healthy".
	ThrottleValid bool
	// Conditions are the host's power-health states active right now, empty
	// when it reports none.
	Conditions []Condition
}

// Condition reports whether c is among this reading's live conditions.
func (r Reading) Condition(c Condition) bool {
	for _, x := range r.Conditions {
		if x == c {
			return true
		}
	}
	return false
}

// Thresholds are the display/alert values a Source's readings should be
// judged against — the domain a UI draws its trace over, and the point past
// which a reading counts as hot — kept with the Source itself so a display
// and the sensor it describes cannot silently disagree.
type Thresholds struct {
	// TempCeilingC is the top of the temperature domain this Source's
	// readings are drawn against.
	TempCeilingC float64
	// TempWarnC is where a reading is treated as running hot — the
	// operator-relevant point (e.g. "the SoC is throttling"), not an
	// arbitrary fraction of the ceiling.
	TempWarnC float64
}

// Source is one board's channel for host self-telemetry.
// pkg/device/bmcsensor implements it for the Raspberry Pi OP-TEE/I2C push
// record; a board with no such channel registers none (see Register).
type Source interface {
	// Latest returns the most recent reading. false means no telemetry has
	// ever been observed — not merely stale — which a consumer distinguishes
	// from Reading.Stale (the channel is live but currently quiet).
	Latest() (Reading, bool)
	// Thresholds are this source's display/alert values.
	Thresholds() Thresholds
}

// Trend is the optional extension a Source implements when it also keeps a
// bounded history of recent readings, for a UI trend graph. Not every
// consumer needs one (the OTel gauges and the IPMI SDR only ever want the
// latest sample) and not every Source can cheaply keep one, so it is kept out
// of Source itself; a consumer that wants a trend type-asserts a registered
// Source for it and falls back to a single current point when a Source does
// not implement it.
type Trend interface {
	Source
	// Trend returns the recent readings, oldest first. Only TempC/TempValid
	// and FanDutyPct/FanValid are meaningful on each entry — At and the
	// rest are a historical point's own instant, not this call's.
	Trend() []Reading
}

// current is the process-wide registry: a single slot, set once from main
// once the board's profile is known. atomic.Pointer rather than a mutex
// because the access pattern is exactly logger.Holder's — many concurrent
// Gets from request/callback goroutines, at most one Set during startup —
// and a pointer swap needs no lock for that.
var current atomic.Pointer[Source]

// Register sets the process-wide Source, overwriting whatever was set
// before. Called once from main, after the board's profile is known; a board
// with no host-telemetry channel simply never calls it, so Get reports none.
//
// Register(nil) clears the registry, so Get reports none again — this is
// mainly for tests that need to restore the "no Source" state a later test
// depends on, since the registry is process-wide.
func Register(s Source) {
	if s == nil {
		current.Store(nil)
		return
	}
	current.Store(&s)
}

// Get returns the registered Source, or false if none has been registered
// (or Register(nil) cleared it).
func Get() (Source, bool) {
	p := current.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}
