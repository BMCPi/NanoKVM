package components

// overview_resources.go backs the Server Overview's Resources card: the model
// the fragment fills in. The traces themselves are drawn by the shared
// sparkline (sparkline.go), which the host-sensors card uses too.

// OverviewResources is the Resources card body.
type OverviewResources struct {
	// Sampling is false until the sampler has enough readings to draw a
	// trend, which is the card's whole content — see the empty state.
	Sampling bool
	CPU      SparkSeries
	Memory   SparkSeries
	Disk     SparkSeries
}

// Series is the rows in the order they are drawn, skipping any subsystem that
// could not be read.
func (m OverviewResources) Series() []SparkSeries {
	return validSeries(m.CPU, m.Memory, m.Disk)
}

// validSeries drops the series whose source could not be read, so a subsystem
// that is absent renders as nothing rather than as a flat zero — a zeroed
// reading is a reading, and says something untrue.
func validSeries(in ...SparkSeries) []SparkSeries {
	out := make([]SparkSeries, 0, len(in))
	for _, s := range in {
		if s.Valid {
			out = append(out, s)
		}
	}
	return out
}
