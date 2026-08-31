package telemetry

// host_sensors.go exports what the managed host reports about itself.
//
// The readings arrive from an OP-TEE pseudo-TA on the Pi, which pushes a
// record into this BMC's emulated I2C EEPROM from the secure world; pkg/bmcsensor
// reads it. Unlike the BMC's own resources, these describe the machine being
// managed, so they are named nanokvm_host_* rather than nanokvm_bmc_*.
//
// Everything is gated on a live, non-stale sample. A powered-off host leaves
// its last record in the EEPROM parsing perfectly, and exporting that would
// give a scraper a plausible die temperature for a machine that is switched
// off — the one failure mode worth going out of the way to avoid here, because
// an alert that stays quiet is indistinguishable from a healthy host.

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/pi-bmc/nanokvm-app/pkg/bmcsensor"
)

// initHostSensorMetrics registers the observable gauges. Called from initMetrics.
func initHostSensorMetrics() {
	m := otel.Meter("github.com/pi-bmc/nanokvm-app")

	var err error
	gauge := func(name, desc, unit string) metric.Float64ObservableGauge {
		g, e := m.Float64ObservableGauge(name,
			metric.WithDescription(desc), metric.WithUnit(unit))
		if e != nil {
			err = e
		}
		return g
	}

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

	if err != nil {
		pkgLog.Warn("telemetry: host sensor instrument creation", slog.Any("err", err))
		return
	}

	bool01 := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		reading, readErr := bmcsensor.Default().Read()
		live := readErr == nil && !reading.Stale
		o.ObserveFloat64(reporting, bool01(live))
		if !live {
			return nil
		}

		if reading.TempValid() {
			o.ObserveFloat64(temp, reading.Celsius())
		}
		if reading.FanValid() {
			o.ObserveFloat64(fanDuty, float64(reading.FanDutyPct))
			// Zero means the host has no tach capture, not a stalled fan.
			if reading.FanRPM > 0 {
				o.ObserveFloat64(fanRPM, float64(reading.FanRPM))
			}
		}
		if reading.ThrottleValid() {
			o.ObserveFloat64(throttle, bool01(reading.Throttled()))
			o.ObserveFloat64(underVoltage, bool01(reading.UnderVoltage()))
			o.ObserveFloat64(freqCapped, bool01(reading.FrequencyCapped()))
			o.ObserveFloat64(softTempLimit, bool01(reading.SoftTempLimited()))
		}
		return nil
	}, temp, fanDuty, fanRPM, throttle, underVoltage, freqCapped, softTempLimit, reporting)
	if err != nil {
		pkgLog.Warn("telemetry: host sensor callback registration", slog.Any("err", err))
	}
}
