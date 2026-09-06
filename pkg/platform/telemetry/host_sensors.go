package telemetry

// host_sensors.go exports what the managed host reports about itself.
//
// The readings come through pkg/device/hostsensor's board-agnostic seam: on
// the Raspberry Pi, pkg/device/bmcsensor is the registered Source, reading a
// record an OP-TEE pseudo-TA pushes into this BMC's emulated I2C EEPROM from
// the secure world. Unlike the BMC's own resources, these describe the
// machine being managed, so they are named nanokvm_host_* rather than
// nanokvm_bmc_*. A board with no registered Source (no host-telemetry
// channel) reports nanokvm_host_sensor_reporting=0 and nothing else — never a
// fabricated temperature or fan reading.
//
// Everything but that one gauge is further gated on a live, non-stale sample.
// A powered-off host leaves its last record in the EEPROM parsing perfectly,
// and exporting that would give a scraper a plausible die temperature for a
// machine that is switched off — the one failure mode worth going out of the
// way to avoid here, because an alert that stays quiet is indistinguishable
// from a healthy host.

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/pi-bmc/nanokvm-app/pkg/device/hostsensor"
)

// initHostSensorMetrics registers the observable gauges. Called from initMetrics.
func initHostSensorMetrics() {
	m := otel.Meter("github.com/pi-bmc/nanokvm-app")

	gf := &gaugeFactory{m: m}
	gauge := gf.float64Gauge

	// No unit in the names: the exporter appends one from WithUnit (see
	// resources.go). "Cel" becomes _celsius, "%" becomes _percent.
	temp := gauge("nanokvm_host_soc_temperature",
		"Managed host SoC die temperature, as reported by OP-TEE over I2C", "Cel")
	fanDuty := gauge("nanokvm_host_fan_duty",
		"Managed host active-cooler PWM duty", "%")
	// "{rpm}" is a UCUM annotation, which the exporter drops rather than
	// turning into a suffix, so the unit is spelled in the name here.
	fanRPM := gauge("nanokvm_host_fan_speed_rpm",
		"Managed host active-cooler tachometer reading; absent where the host has no tach capture", "{rpm}")

	// The throttle word as four 0/1 gauges. Booleans rather than a bitmask
	// because a scraper cannot alert on a bit position, and these are exactly
	// the conditions worth alerting on: the PMIC browning out, and the SoC
	// capping itself. The latched-since-boot halves are deliberately not
	// exported — a gauge that goes to 1 and stays there forever is a worse
	// version of what a scraper already does with the live one.
	//
	// No unit on any of them. The obvious "1" is UCUM's dimensionless unit and
	// the exporter renders it as _ratio, which is exactly the wrong word for a
	// boolean: nanokvm_host_throttled_ratio reads as a fraction of time spent
	// throttled rather than a state.
	throttle := gauge("nanokvm_host_throttled",
		"1 while the managed host's SoC is being throttled", "")
	underVoltage := gauge("nanokvm_host_under_voltage",
		"1 while the managed host's PMIC reports under-voltage", "")
	freqCapped := gauge("nanokvm_host_frequency_capped",
		"1 while the managed host's clock is capped", "")
	softTempLimit := gauge("nanokvm_host_soft_temp_limited",
		"1 while the managed host is at its soft temperature limit", "")

	// Reported for every sample, live or not, so a scraper can tell "the host
	// is quiet" from "the BMC stopped scraping". This one is not gated.
	reporting := gauge("nanokvm_host_sensor_reporting",
		"1 while the managed host is pushing sensor records, 0 when it has gone quiet", "")

	if gf.err != nil {
		pkgLog().Warn("telemetry: host sensor instrument creation", slog.Any("err", gf.err))
		return
	}

	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := sampleHostSensors()
		o.ObserveFloat64(reporting, s.reporting)
		if s.hasTemp {
			o.ObserveFloat64(temp, s.temp)
		}
		if s.hasFan {
			o.ObserveFloat64(fanDuty, s.fanDuty)
			if s.hasFanRPM {
				o.ObserveFloat64(fanRPM, s.fanRPM)
			}
		}
		if s.hasThrottle {
			o.ObserveFloat64(throttle, s.throttle)
			o.ObserveFloat64(underVoltage, s.underVoltage)
			o.ObserveFloat64(freqCapped, s.freqCapped)
			o.ObserveFloat64(softTempLimit, s.softTempLimit)
		}
		return nil
	}, temp, fanDuty, fanRPM, throttle, underVoltage, freqCapped, softTempLimit, reporting)
	if err != nil {
		pkgLog().Warn("telemetry: host sensor callback registration", slog.Any("err", err))
	}
}

// hostSensorSample is what one collection interval observes about the
// managed host, computed from the registered hostsensor.Source (if any) by
// sampleHostSensors. Pulled out of the RegisterCallback closure above so the
// with/without-a-registered-Source behavior is unit-testable without driving
// the OTel/Prometheus collection pipeline.
type hostSensorSample struct {
	// reporting is always set: 1 while a registered Source has a live,
	// non-stale reading, 0 otherwise (no Source at all, or one that is
	// absent/stale) — the one gauge exported "live or not", per the file
	// comment.
	reporting float64

	temp      float64
	hasTemp   bool
	fanDuty   float64
	hasFan    bool
	fanRPM    float64
	hasFanRPM bool

	hasThrottle                                       bool
	throttle, underVoltage, freqCapped, softTempLimit float64
}

// sampleHostSensors reads the registered hostsensor.Source, if any, and
// reports what this interval's gauges should observe. No Source registered,
// or a Source with nothing live to report, both come back as a zero
// hostSensorSample (reporting: 0, nothing else set) — honest absence, never
// a fabricated reading.
func sampleHostSensors() hostSensorSample {
	source, registered := hostsensor.Get()
	if !registered {
		return hostSensorSample{}
	}

	reading, sampled := source.Latest()
	if !sampled || reading.Stale {
		return hostSensorSample{}
	}

	s := hostSensorSample{reporting: 1}
	if reading.TempValid {
		s.temp, s.hasTemp = reading.TempC, true
	}
	if reading.FanValid {
		s.fanDuty, s.hasFan = reading.FanDutyPct, true
		if reading.FanRPMValid {
			s.fanRPM, s.hasFanRPM = float64(reading.FanRPM), true
		}
	}
	if reading.ThrottleValid {
		s.hasThrottle = true
		s.throttle = bool01(reading.Condition(hostsensor.ConditionThrottled))
		s.underVoltage = bool01(reading.Condition(hostsensor.ConditionUnderVoltage))
		s.freqCapped = bool01(reading.Condition(hostsensor.ConditionFreqCapped))
		s.softTempLimit = bool01(reading.Condition(hostsensor.ConditionSoftTempLimit))
	}
	return s
}

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
