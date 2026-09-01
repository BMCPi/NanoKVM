package bmcsensor

// hostsensor.go adapts this package's shared Sampler onto
// pkg/device/hostsensor.Source (and its optional Trend extension), so main
// can register it as the process's host-telemetry channel when the Raspberry
// Pi's OP-TEE/I2C push record is present. See pkg/device/hostsensor for why
// the seam exists and what a board with no such channel (registering
// nothing) gets instead.

import "github.com/pi-bmc/nanokvm-app/pkg/device/hostsensor"

// RPi thermal domain: the display ceiling and the point at which the SoC
// begins capping itself. Moved here from the UI (ui/components's
// overview_host_sensors.go used to hardcode 100/80) because they describe
// this Source's own sensor, not anything about how a card draws it — a
// future Source states its own via Thresholds.
const (
	// hostTempCeilingC is the top of the temperature domain the overview
	// card draws its trace against. A Pi 5 hard-caps well below it, so
	// nothing ever leaves the box.
	hostTempCeilingC = 100
	// hostTempWarnC is where the SoC begins capping itself: the
	// operator-relevant threshold ("is it throttling"), not an arbitrary
	// fraction of the ceiling.
	hostTempWarnC = 80
)

// Thresholds implements hostsensor.Source.
func (s *Sampler) Thresholds() hostsensor.Thresholds {
	return hostsensor.Thresholds{TempCeilingC: hostTempCeilingC, TempWarnC: hostTempWarnC}
}

// Latest implements hostsensor.Source over the shared sampler's most recent
// sample. false covers every read failure the same way — ErrNoRecord (the
// ordinary state before the host boots past its firmware) as much as a torn
// or corrupted record — because hostsensor.Source carries no error, only
// "was there a trustworthy reading" (see the Sample/Start ticker for where
// the distinctions are still logged).
func (s *Sampler) Latest() (hostsensor.Reading, bool) {
	reading, err := s.Read()
	if err != nil {
		return hostsensor.Reading{}, false
	}
	return toHostReading(reading), true
}

// Trend implements hostsensor.Trend over the sampler's bounded history, for
// the overview card's spark graphs. Only TempC/TempValid and
// FanDutyPct/FanValid are meaningful on each entry: SensorPoint keeps no
// time.Time (only a display string already formatted for the chart axis), so
// At is left zero on every point here.
func (s *Sampler) Trend() []hostsensor.Reading {
	history := s.History()
	if len(history) == 0 {
		return nil
	}
	out := make([]hostsensor.Reading, len(history))
	for i, p := range history {
		out[i] = hostsensor.Reading{
			TempC:      p.TempC,
			TempValid:  p.TempValid,
			FanDutyPct: p.FanDutyPct,
			FanValid:   p.FanValid,
		}
	}
	return out
}

// toHostReading adapts this package's wire-format Reading (a Record plus
// this side's staleness bookkeeping) onto the board-agnostic Reading every
// hostsensor consumer understands.
func toHostReading(r Reading) hostsensor.Reading {
	out := hostsensor.Reading{
		At:            r.At,
		Stale:         r.Stale,
		TempC:         r.Celsius(),
		TempValid:     r.TempValid(),
		FanDutyPct:    float64(r.FanDutyPct),
		FanValid:      r.FanValid(),
		FanLevel:      r.FanLevel,
		FanMaxLevel:   r.FanMaxLevel,
		FanRPM:        r.FanRPM,
		FanRPMValid:   r.FanRPMValid(),
		ThrottleValid: r.ThrottleValid(),
	}
	for _, c := range r.LiveConditions() {
		out.Conditions = append(out.Conditions, hostsensor.Condition(c))
	}
	return out
}

// Compile-time assertions that Sampler satisfies both the base Source and
// the Trend extension — the whole point of this file.
var (
	_ hostsensor.Source = (*Sampler)(nil)
	_ hostsensor.Trend  = (*Sampler)(nil)
)
