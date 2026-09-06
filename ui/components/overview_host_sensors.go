package components

// overview_host_sensors.go backs the Server Overview's Host Sensors card: what
// the managed host reports about itself, as opposed to what the BMC measures
// about the BMC (see overview_resources.go).
//
// The readings come through pkg/device/hostsensor's board-agnostic seam. On
// the Raspberry Pi that seam's registered Source is an OP-TEE pseudo-TA,
// which pushes a record into this BMC's emulated I2C EEPROM from the secure
// world — the only live telemetry the host offers, since its firmware is
// UEFI and there is no OS agent to ask, which is why this card exists rather
// than the numbers being folded into Server Information. The temperature
// domain (the trace's ceiling and its throttle-coloured point) is no longer
// fixed here: it comes from the registered Source's own Thresholds, so a
// future board's Source states its own numbers instead of inheriting the
// Pi's.

// OverviewHostSensors is the Host Sensors card body.
type OverviewHostSensors struct {
	// Available is false where the board has no slave EEPROM at all — the
	// DTS or the kernel config is missing it — which is a different thing
	// from a host that has not pushed yet.
	Available bool
	// Reporting is false once the host's sequence number stops advancing.
	// The trace is still worth drawing then (it shows what happened before
	// the host went quiet) but the readings are not current, and the card
	// has to say so rather than let a frozen number read as live.
	Reporting bool
	// Sampling is false until there are enough readings to draw a trend.
	Sampling bool

	Temperature SparkSeries
	Fan         SparkSeries

	// Throttles are the host's live power-health conditions, empty when it
	// reports none. The latched-since-boot flags are deliberately not shown:
	// a badge that appears at the first brownout and never clears again
	// stops carrying information within a day.
	Throttles []string
	// ThrottleKnown is false on a host whose firmware predates the throttle
	// word, where "no conditions" and "not reported" would otherwise look
	// identical.
	ThrottleKnown bool
}

// Series is the traces in the order they are drawn, skipping any the host did
// not report.
func (m OverviewHostSensors) Series() []SparkSeries {
	return validSeries(m.Temperature, m.Fan)
}

// Healthy reports whether the host is reporting and no throttle condition is
// live — the one-glance answer the card's badge gives.
func (m OverviewHostSensors) Healthy() bool {
	return m.Reporting && m.ThrottleKnown && len(m.Throttles) == 0
}
