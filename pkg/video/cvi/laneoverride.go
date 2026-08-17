package cvi

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// pollInterval is how often the supervisor asks the bridge what it is seeing.
// Each poll opens the bridge's register window, which halts the firmware
// driving its CSI transmitter, so the rate is a real constraint on the stream
// rather than a matter of taste. NANOKVM_SIGNAL_POLL exists to measure that.
func pollInterval() time.Duration { return envDuration("NANOKVM_SIGNAL_POLL", signalPoll) }

// streamPollInterval is the same question for a pipeline that is already up,
// where a poll costs a break in the stream rather than just a stall.
func streamPollInterval() time.Duration { return envDuration("NANOKVM_STREAM_POLL", streamPoll) }

// viChnDepth is how many finished frames VI holds for a reader on its channel.
//
// Zero in normal operation, and that is not a tuning choice: frames leave VI
// through its bind to VPSS, so a depth here is a queue nobody drains -- it
// would hold VB blocks out of circulation and eventually starve the receiver
// that fills them.
//
// NANOKVM_VI_DEPTH opens the tap. VI.GetChnFrame is the only way to see what
// the receiver actually wrote, before the scaler has touched it and before the
// encoder has had an opinion, and those are otherwise indistinguishable from
// each other: every one of them presents as a black picture. Set it to 1 or 2
// when the question is "is there a real image on the CSI link at all".
func viChnDepth() uint64 {
	v, ok := envUint("NANOKVM_VI_DEPTH")
	if !ok || v > 8 { // the driver documents the range as [0, 8]
		return 0
	}
	return v
}

func envDuration(name string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Board bring-up overrides. The LT6911's lane routing, MAC clock and D-PHY
// settle time are properties of the board, not of the part, and getting them
// wrong presents identically to having no signal at all: the receiver simply
// never raises an interrupt. Reading them from the environment makes a
// candidate testable in one deploy instead of one rebuild each.
func envUint(name string) (uint64, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(raw), 0, 32)
	if err != nil {
		return 0, false
	}
	return v, true
}

// applyLaneOverride replaces the lane map from NANOKVM_LANES, given as five
// comma-separated entries, e.g. "2,4,3,-1,-1". Entry 0 is the clock lane and
// the rest are data; a negative entry marks a lane the board does not wire,
// which is how the driver derives the lane count.
func applyLaneOverride(mipi *MipiDevAttr) {
	raw, ok := os.LookupEnv("NANOKVM_LANES")
	if !ok {
		return
	}
	parts := strings.Split(raw, ",")
	if len(parts) != len(mipi.Lane_id) {
		return
	}
	var lanes [5]int16
	for i, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 16)
		if err != nil {
			return
		}
		lanes[i] = int16(v)
	}
	mipi.Lane_id = lanes
}
