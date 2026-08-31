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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/network"
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

	done     chan struct{}
	stopOnce sync.Once
}

var (
	activeMu sync.Mutex
	active   *Service
)

// Start launches the sync loop when timesync is enabled in config. Safe to
// call before the network is up — the loop retries with backoff until a
// source is reachable.
func Start() {
	cfg := config.GetInstance().TimeSync
	if !cfg.Enabled {
		slog.Info("timesync: disabled by config")
		return
	}

	s := &Service{
		servers:  cfg.Servers,
		interval: time.Duration(cfg.IntervalMinutes) * time.Minute,
		rtc:      openRTC(),
		done:     make(chan struct{}),
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

// Stop terminates the sync loop. Idempotent.
func Stop() {
	activeMu.Lock()
	s := active
	active = nil
	activeMu.Unlock()
	if s != nil {
		s.stop()
	}
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

func (s *Service) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
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
		slog.Warn("timesync: read rtc failed", slog.Any("err", err))
		return
	}
	if !t.After(time.Now()) {
		return
	}
	if err := setSystemTime(t); err != nil {
		slog.Warn("timesync: seed clock from rtc failed", slog.Any("err", err))
		return
	}
	slog.Info("timesync: seeded clock from rtc", slog.Time("time", t))
}

func (s *Service) loop() {
	backoff := retryFloor
	timer := time.NewTimer(0) // main gates startup on network.WaitReady; try immediately
	defer timer.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-timer.C:
		}

		if err := s.sync(); err != nil {
			slog.Warn("timesync: sync failed", slog.Any("err", err), slog.Duration("retryIn", backoff))
			timer.Reset(backoff)
			backoff = min(backoff*2, retryCap)
			continue
		}
		backoff = retryFloor
		timer.Reset(s.interval)
	}
}

// sync tries each time source in order and applies the first answer to the
// system clock and the RTC.
func (s *Service) sync() error {
	sources := []struct {
		name  string
		query func() (time.Time, bool)
	}{
		{"config", func() (time.Time, bool) { return queryNTP(s.servers) }},
		{"dhcp", func() (time.Time, bool) { return queryNTP(network.DHCPNTPServers()) }},
		{"fallback-ip", func() (time.Time, bool) { return queryNTP(fallbackNTPIPs) }},
		{"fallback-dns", func() (time.Time, bool) { return queryNTP(fallbackNTPHostnames) }},
		{"http", func() (time.Time, bool) { return queryHTTP(fallbackHTTPURLs) }},
	}

	for _, src := range sources {
		now, ok := src.query()
		if !ok {
			continue
		}
		if err := setSystemTime(now); err != nil {
			return err
		}
		if s.rtc != nil {
			if err := s.rtc.set(now); err != nil {
				slog.Warn("timesync: write rtc failed", slog.Any("err", err))
			}
		}
		s.mu.Lock()
		first := s.lastSync.IsZero()
		s.lastSync, s.source = now, src.name
		s.mu.Unlock()
		if first {
			slog.Info("timesync: clock set", slog.Time("time", now), slog.String("source", src.name))
		} else {
			slog.Debug("timesync: clock refreshed", slog.String("source", src.name))
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
