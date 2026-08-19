// Package autoupdate runs a background ticker that polls upstream release
// metadata and applies updates when newer versions are available. Driven by
// config.AutoUpdate; opt-in (Enabled defaults to false).
//
// The service is restart-safe: changes to config (via Settings() / SetSettings()
// during a server run, or via /etc/kvm/server.yaml across restarts) take
// effect on the next tick. The ticker holds no state beyond the running
// goroutine itself.
package autoupdate

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/config"

	log "github.com/sirupsen/logrus"
)

// minInterval is the floor for how often we hit upstream version APIs.
// Clamps misconfigured low values so users can't accidentally hammer GitHub.
const minInterval = 5 * time.Minute

// preRestartDelay is the wait between a successful application update and
// the service restart kick, giving the HTTP response a chance to flush.
const preRestartDelay = 1 * time.Second

var (
	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
)

// Start launches the background ticker if AutoUpdate.Enabled is true.
// Safe to call multiple times — repeated calls cancel any existing ticker
// and restart with the current config. Returns immediately.
//
// ctx is the process-lifetime context: the ticker, and any update running on
// it, stop when the server shuts down. Callers restarting the ticker after a
// config change (the settings UI, the autoupdate API) pass the same one, which
// they reach through deps.
func Start(ctx context.Context) {
	mu.Lock()
	defer mu.Unlock()

	if running {
		// Stop the existing goroutine before starting a fresh one with the
		// (possibly updated) config.
		cancel()
		running = false
	}

	cfg := config.GetInstance().AutoUpdate
	if !cfg.Enabled {
		log.Info("autoupdate: disabled by config")
		return
	}

	loopCtx, c := context.WithCancel(ctx)
	cancel = c
	running = true

	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	if interval < minInterval {
		interval = minInterval
	}

	go loop(loopCtx, interval)
	log.Infof("autoupdate: enabled (interval=%s, application=%v)",
		interval, cfg.Application)
}

// Stop cancels the background ticker. Safe to call when not running.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if !running {
		return
	}
	cancel()
	running = false
	log.Info("autoupdate: stopped")
}

// loop is the worker goroutine: an initial check after one interval (so the
// process gets a chance to settle), then once per interval, until ctx is
// cancelled. Each tick re-reads config so toggling Application/BIOS from
// the UI takes effect without a restart.
func loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce(ctx)
		}
	}
}

// runOnce performs a single check + apply pass. Errors are logged but
// don't abort the loop — a transient GitHub outage or network blip should
// not silently disable the updater forever.
func runOnce(ctx context.Context) {
	cfg := config.GetInstance().AutoUpdate

	if cfg.Application {
		if err := applyAppUpdateIfNewer(ctx); err != nil {
			log.Warnf("autoupdate: application: %v", err)
		}
	}
}

func applyAppUpdateIfNewer(ctx context.Context) error {
	current := normaliseVersion(application.CurrentVersion())
	latest := normaliseVersion(application.LatestVersion(ctx))
	if latest == "" || latest == current {
		return nil
	}
	log.Infof("autoupdate: application update available (%s → %s)", current, latest)

	if err := application.RunUpdate(ctx); err != nil {
		return err
	}
	log.Info("autoupdate: application update applied; restarting service")
	time.Sleep(preRestartDelay)
	application.RestartService()
	return nil
}

// normaliseVersion strips a leading "v" so "v1.2.3" and "1.2.3" compare equal.
func normaliseVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
