package components

// overview_host_sensors.go backs the Server Overview's Host Sensors card: what
// the managed host reports about itself, as opposed to what the BMC measures
// about the BMC (see overview_resources.go).
//
// The readings come from an OP-TEE pseudo-TA on the Pi, which pushes a record
// into this BMC's emulated I2C EEPROM from the secure world. That path is the
// only live telemetry the host offers — its firmware is UEFI, so there is no
// OS agent to ask — which is why this card exists rather than the numbers
// being folded into Server Information.

// SoC domain. The trace is drawn against a fixed 0..100 °C so a reading means
// the same thing between visits and across reboots, and so cooling can be read
// against temperature on the same box.
const (
	// hostTempCeiling is the top of the temperature domain. A Pi 5 hard-caps
	// well below it, so nothing ever leaves the box.
	hostTempCeiling = 100
	// hostTempThrottle is where the SoC begins capping itself. It is the
	// reference the trace is drawn against, and the point at which the
	// reading is coloured — the operator-relevant threshold is "is it
	// throttling", not an arbitrary fraction of the ceiling.
	hostTempThrottle = 80
)

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

// HostTempCeiling and HostTempThrottle are exported for the fragment that
// builds the series, so the domain and the threshold are stated once.
func HostTempCeiling() float64  { return hostTempCeiling }
func HostTempThrottle() float64 { return hostTempThrottle }
