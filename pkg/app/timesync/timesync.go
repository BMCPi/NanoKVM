// Package timesync keeps the BMC's clock correct without shipping ntpd on the
// image. It is a simplified port of JetKVM's internal/timesync: one SNTP
// client (beevik/ntp) plus an HTTP Date-header fallback, trying sources in a
// fixed order — operator-configured servers, servers from the eth0 DHCP lease,
// well-known anycast IPs (works before DNS), well-known hostnames, and finally
// HTTP time from captive-portal probe URLs (works where UDP/123 is blocked).
//
// Differences from JetKVM, beyond dropping its per-server Prometheus metrics
// and configurable source ordering: the system clock is set with
// settimeofday(2) directly instead of exec'ing `date -s`, and the DHCP NTP
// servers come from the app's own in-process DHCP client (network package)
// rather than an external nmlite. When a battery-less reboot left the clock at
// the epoch, the RTC (if the kernel exposes one) seeds the clock at startup
// and is written back after every successful sync, exactly like JetKVM.
package timesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/app/network"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

const (
	// retryFloor/retryCap bound the backoff between failed sync attempts. The
	// floor matches JetKVM's retry step; the cap keeps a dead uplink from
	// being probed more than every few minutes.
	retryFloor = 5 * time.Second
	retryCap   = 5 * time.Minute
	// queryTimeout bounds one NTP or HTTP query; queryParallel is how many
	// servers of a source are raced at once (first valid answer wins).
	queryTimeout  = 2 * time.Second
	queryParallel = 4
)

// Service owns the background sync loop.
type Service struct {
	servers  []string // operator-configured, tried before everything else
	interval time.Duration
	rtc      *rtc // nil when the kernel exposes no RTC

	mu       sync.Mutex
	lastSync time.Time
	source   string

	// ctx is derived (via WithCancel) from the ctx Start was given, so it is
	// cancelled both by the process shutting down and by stop() -- see
	// loop() and sync(), which thread it into every query so a cancellation
	// aborts an in-flight sync rather than letting it run to completion (up
	// to ~15-20s) and step the clock after a Restart's replacement Service
	// has already synced.
	ctx      context.Context
	cancel   context.CancelFunc
	loopDone chan struct{} // closed when loop() returns; stop() joins on it
	stopOnce sync.Once

	log *slog.Logger
}

var (
	activeMu sync.Mutex
	active   *Service
)

// pkgLogHolder is pkg/timesync's holder for the "timesync" component logger,
// needed by ntp.go and http.go's query helpers: they run in goroutines
// spawned by a Service's sync loop and by Restart, which reuses whatever
// logger the most recent Start call was given rather than a bare default —
// see logger.Holder's doc comment for why a sync.Once-guarded var would get
// either of those wrong.
var pkgLogHolder logger.Holder

// pkgLog returns the package's component logger, defaulting to the process
// logger if Start has not run yet.
func pkgLog() *slog.Logger {
	return pkgLogHolder.Get()
}

// Start launches the sync loop when timesync is enabled in config. Safe to
// call before the network is up — the loop retries with backoff until a
// source is reachable.
//
// ctx is normally the process-lifetime context: the Service derives its own
// ctx from it, so process shutdown alone cancels an in-flight sync even
// without an explicit Stop.
func Start(ctx context.Context, log *slog.Logger) {
	log = logger.Or(log)
	pkgLogHolder.Set(log)

	cfg := config.GetInstance().TimeSync
	if !cfg.Enabled {
		log.Info("timesync: disabled by config")
		return
	}

	sctx, cancel := context.WithCancel(ctx)
	s := &Service{
		servers:  cfg.Servers,
		interval: time.Duration(cfg.IntervalMinutes) * time.Minute,
		rtc:      openRTC(),
		ctx:      sctx,
		cancel:   cancel,
		loopDone: make(chan struct{}),
		log:      log,
	}
	s.seedFromRTC()

	activeMu.Lock()
	defer activeMu.Unlock()
	if active != nil {
		active.stop()
	}
	active = s
	go s.loop()
}

// Stop terminates the sync loop and joins it, so a sync already in progress
// cannot touch the clock after Stop returns. Idempotent.
func Stop() {
	activeMu.Lock()
	s := active
	active = nil
	activeMu.Unlock()
	if s != nil {
		s.stop()
	}
}

// Restart re-reads config and rebuilds the sync loop, reusing the logger
// Start was given rather than falling back to a bare default. ctx is the
// caller's process-lifetime context (matches autoupdate.Restart(ctx), the
// package's single convention). Stop before Start because Start returns
// early when the subsystem is disabled — without the Stop, turning timesync
// off would leave the previous loop running and still touching the clock;
// Stop also joins the old loop, so its retired Service cannot still be
// mid-sync when the new one starts (which is what let a stale sync silently
// step the clock after the new Service had already synced).
func Restart(ctx context.Context) {
	log := pkgLog()

	Stop()
	Start(ctx, log)
}

// Status reports the last successful sync, its source, and whether the clock
// has been synchronized at all this boot.
func Status() (lastSync time.Time, source string, synced bool) {
	activeMu.Lock()
	s := active
	activeMu.Unlock()
	if s == nil {
		return time.Time{}, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync, s.source, !s.lastSync.IsZero()
}

// stop cancels the Service's ctx and waits for loop() to return before
// closing the RTC, so Stop/Restart JOIN the loop instead of merely signalling
// it -- a bare signal let the previous implementation return while the old
// loop's sync() (up to ~15-20s worst case) was still in flight, free to call
// setSystemTime after a just-started replacement Service had already synced.
func (s *Service) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		<-s.loopDone
		if s.rtc != nil {
			s.rtc.close()
		}
	})
}

// seedFromRTC sets the system clock from the RTC when the RTC is ahead of it.
// Without a backup battery the RTC restarts at its epoch on cold boot, in
// which case this is a no-op; on a warm reboot it restores a sane clock long
// before the network is up.
func (s *Service) seedFromRTC() {
	if s.rtc == nil {
		return
	}
	t, err := s.rtc.read()
	if err != nil {
		s.log.Warn("timesync: read rtc failed", slog.Any("err", err))
		return
	}
	if !t.After(time.Now()) {
		return
	}
	if err := setSystemTime(t); err != nil {
		s.log.Warn("timesync: seed clock from rtc failed", slog.Any("err", err))
		return
	}
	s.log.Info("timesync: seeded clock from rtc", slog.Time("time", t))
}

func (s *Service) loop() {
	defer close(s.loopDone)

	backoff := retryFloor
	timer := time.NewTimer(0) // main gates startup on network.WaitReady; try immediately
	defer timer.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}

		if err := s.sync(s.ctx); err != nil {
			if s.ctx.Err() != nil {
				// Shutting down: sync() returned because it was cancelled,
				// not because every source failed. Nothing to retry.
				return
			}
			s.log.Warn("timesync: sync failed", slog.Any("err", err), slog.Duration("retryIn", backoff))
			timer.Reset(backoff)
			backoff = min(backoff*2, retryCap)
			continue
		}
		backoff = retryFloor
		timer.Reset(s.interval)
	}
}

// sync tries each time source in order and applies the first answer to the
// system clock and the RTC. ctx bounds every query (see http.go/ntp.go) and
// is checked between sources too, so a cancellation abandons the attempt
// promptly instead of working through the whole source list first.
func (s *Service) sync(ctx context.Context) error {
	sources := []struct {
		name  string
		query func() (time.Time, bool)
	}{
		{"config", func() (time.Time, bool) { return queryNTP(ctx, s.servers) }},
		{"dhcp", func() (time.Time, bool) { return queryNTP(ctx, network.DHCPNTPServers()) }},
		{"fallback-ip", func() (time.Time, bool) { return queryNTP(ctx, fallbackNTPIPs) }},
		{"fallback-dns", func() (time.Time, bool) { return queryNTP(ctx, fallbackNTPHostnames) }},
		{"http", func() (time.Time, bool) { return queryHTTP(ctx, fallbackHTTPURLs) }},
	}

	for _, src := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now, ok := src.query()
		if !ok {
			continue
		}
		if err := setSystemTime(now); err != nil {
			return err
		}
		if s.rtc != nil {
			if err := s.rtc.set(now); err != nil {
				s.log.Warn("timesync: write rtc failed", slog.Any("err", err))
			}
		}
		s.mu.Lock()
		first := s.lastSync.IsZero()
		s.lastSync, s.source = now, src.name
		s.mu.Unlock()
		if first {
			s.log.Info("timesync: clock set", slog.Time("time", now), slog.String("source", src.name))
		} else {
			s.log.Debug("timesync: clock refreshed", slog.String("source", src.name))
		}
		return nil
	}
	return errors.New("no time source reachable")
}

func setSystemTime(t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	return nil
}
