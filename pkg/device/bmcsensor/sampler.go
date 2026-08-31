package bmcsensor

// sampler.go is the one continuously-polling reader of the host's record.
//
// Two jobs. It keeps a bounded history, because the record carries a single
// instant and the UI draws a trend. And it is the process's single Reader, so
// every consumer — the Redfish Sensor and Thermal views, the IPMI sensor HAL,
// the drawer's graphs — reports the same sample and the same staleness.
//
// That second job fixes a real defect. A Reader measures staleness by watching
// the sequence number change between its own reads (see reader.go), so a
// consumer that reads rarely sets lastSeqAt on its first read and concludes the
// sample is fresh — a host that has been powered off for an hour reads as
// live, with a plausible die temperature to match. A reader that never stops
// looking cannot make that mistake, and one shared instance means nobody else
// can either.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/platform/ring"
)

const (
	// sensorInterval between polls. The record is 48 bytes read out of a
	// sysfs attribute, so the cost is a syscall; the ceiling on usefulness is
	// how often the pTA pushes, which is on the order of seconds.
	sensorInterval = 10 * time.Second

	// sensorDepth is how many points are kept — 180 x 10s = thirty minutes,
	// matching the BMC's own resource history so the two graphs in the drawer
	// cover the same span.
	sensorDepth = 180
)

// SensorPoint is one plotted sample of the host's reported state.
type SensorPoint struct {
	// At is BMC wall time, formatted for the chart's axis.
	At string `json:"at"`
	// TempC is the SoC die temperature. TempValid is false when the pTA
	// carried a previous value forward rather than reading the sensor, in
	// which case this is not a reading and must not be drawn as one.
	TempC      float64 `json:"tempC"`
	TempValid  bool    `json:"tempValid"`
	FanDutyPct float64 `json:"fanDutyPct"`
	FanValid   bool    `json:"fanValid"`
}

// Sampler polls a Reader and remembers what it saw.
//
// Safe for concurrent use. Construct with NewSampler, or use Default for the
// process-wide instance every consumer shares.
type Sampler struct {
	reader *Reader
	now    func() time.Time

	mu      sync.Mutex
	points  ring.Ring[SensorPoint]
	last    Reading
	lastErr error
	haveAny bool
	started bool
}

// NewSampler wraps a Reader. It does not start polling; call Start.
func NewSampler(r *Reader) *Sampler {
	return &Sampler{reader: r, now: time.Now, points: ring.NewRing[SensorPoint](sensorDepth)}
}

var (
	defaultOnce    sync.Once
	defaultSampler *Sampler
)

// Default is the process-wide sampler over the default EEPROM path. Every
// consumer of the host's record goes through this one instance — see the file
// comment for why sharing is not merely an optimisation.
func Default() *Sampler {
	defaultOnce.Do(func() { defaultSampler = NewSampler(NewReader()) })
	return defaultSampler
}

// Available reports whether the slave EEPROM attribute exists at all.
func (s *Sampler) Available() bool { return s.reader.Available() }

// Start polls until ctx is cancelled. Safe to call more than once; only the
// first call starts a goroutine.
func (s *Sampler) Start(ctx context.Context, log *slog.Logger) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	// One sample up front so the first consumer after boot has something, and
	// so the sequence number is observed as early as possible — staleness is
	// measured from that observation, and every second of delay is a second a
	// dead host would read as live.
	if _, err := s.Sample(); err != nil && log != nil {
		log.DebugContext(ctx, "bmcsensor: first sample", slog.Any("err", err))
	}

	go func() {
		ticker := time.NewTicker(sensorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// The error is already recorded on the sampler for Read to
				// return; a tick that could not read is not worth a log line
				// every ten seconds on a board with no slave EEPROM.
				_, _ = s.Sample()
			}
		}
	}()

	if log != nil {
		log.DebugContext(ctx, "bmcsensor: sampling the host record",
			slog.Duration("interval", sensorInterval),
			slog.Duration("history", sensorDepth*sensorInterval))
	}
}

// Sample takes one reading and records it. Exported for the tests and for the
// first call in Start; the ticker does the rest.
func (s *Sampler) Sample() (Reading, error) {
	// Read outside the lock: it opens a file, and holding the mutex across
	// that would block every consumer on the filesystem.
	reading, err := s.reader.Read()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.last, s.lastErr, s.haveAny = reading, err, true
	if err != nil {
		return reading, err
	}

	// A stale sample does not extend the trace. The EEPROM is RAM on this
	// side, so a powered-off host's last record sits there parsing perfectly;
	// plotting it would draw a die temperature for a machine that is off,
	// which is the same live-looking lie the Redfish views refuse to tell. The
	// trace freezes where the host went quiet, which is the truth.
	if reading.Stale {
		return reading, nil
	}

	s.points.Append(SensorPoint{
		At:         s.now().Format("15:04"),
		TempC:      reading.Celsius(),
		TempValid:  reading.TempValid(),
		FanDutyPct: float64(reading.FanDutyPct),
		FanValid:   reading.FanValid(),
	})
	return reading, nil
}

// Read returns the latest sample, with staleness recomputed against the
// current time.
//
// The recomputation matters: Stale is a function of how long ago the sequence
// last moved, so a value frozen when the sample was taken would keep saying
// "live" for a whole interval after it stopped being true.
//
// Before the first sample it reads through synchronously, so a consumer that
// arrives ahead of the sampler — the first Redfish GET after boot — still gets
// an answer rather than a zero Reading.
func (s *Sampler) Read() (Reading, error) {
	s.mu.Lock()
	have, reading, err := s.haveAny, s.last, s.lastErr
	s.mu.Unlock()

	if !have {
		return s.Sample()
	}
	if err != nil {
		return reading, err
	}
	reading.Stale = s.now().Sub(reading.At) > s.reader.staleAfter
	return reading, nil
}

// History returns the recorded points, oldest first. Empty until there are two,
// because one point is a value and the graphs draw trends.
func (s *Sampler) History() []SensorPoint {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.points.Len() < 2 {
		return nil
	}
	return s.points.Snapshot()
}
