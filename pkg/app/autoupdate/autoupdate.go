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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
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

// pkgLogHolder is pkg/autoupdate's holder for the "autoupdate" component
// logger, needed by Stop and Restart: neither is handed a logger of its own,
// and both must reuse whatever logger the most recent Start call was given
// rather than a bare default — see logger.Holder's doc comment for why a
// sync.Once-guarded var would get this wrong.
var pkgLogHolder logger.Holder

// pkgLog returns the package's component logger, defaulting to the process
// logger if Start has not run yet.
func pkgLog() *slog.Logger {
	return pkgLogHolder.Get()
}

// Start launches the background ticker if AutoUpdate.Enabled is true.
// Safe to call multiple times — repeated calls cancel any existing ticker
// and restart with the current config. Returns immediately.
//
// ctx is the process-lifetime context: the ticker, and any update running on
// it, stop when the server shuts down. Callers restarting the ticker after a
// config change (the settings UI, the autoupdate API) go through Restart,
// which reuses this logger rather than deriving a new one.
func Start(ctx context.Context, log *slog.Logger) {
	log = logger.Or(log)
	pkgLogHolder.Set(log)

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
		log.InfoContext(ctx, "autoupdate: disabled by config")
		return
	}

	loopCtx, c := context.WithCancel(ctx)
	cancel = c
	running = true

	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	if interval < minInterval {
		interval = minInterval
	}

	go loop(loopCtx, interval, log)
	log.InfoContext(ctx, "autoupdate: enabled",
		slog.Duration("interval", interval), slog.Bool("application", cfg.Application))
}

// Restart re-applies the current config, reusing the logger Start was given.
// For callers that re-read settings after a config change (the settings UI,
// the autoupdate API) and have no component-tagged logger of their own to
// pass.
func Restart(ctx context.Context) {
	Start(ctx, pkgLog())
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
	pkgLog().Info("autoupdate: stopped")
}

// loop is the worker goroutine: an initial check after one interval (so the
// process gets a chance to settle), then once per interval, until ctx is
// cancelled. Each tick re-reads config so toggling Application/BIOS from
// the UI takes effect without a restart.
func loop(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce(ctx, log)
		}
	}
}

// runOnce performs a single check + apply pass. Errors are logged but
// don't abort the loop — a transient GitHub outage or network blip should
// not silently disable the updater forever.
func runOnce(ctx context.Context, log *slog.Logger) {
	cfg := config.GetInstance().AutoUpdate

	if cfg.Application {
		if err := applyAppUpdateIfNewer(ctx, log); err != nil {
			log.WarnContext(ctx, "autoupdate: application update failed", slog.Any("err", err))
		}
	}
}

func applyAppUpdateIfNewer(ctx context.Context, log *slog.Logger) error {
	current := normaliseVersion(application.CurrentVersion())
	latest := normaliseVersion(application.LatestVersion(ctx, log))
	if latest == "" || latest == current {
		return nil
	}
	log.InfoContext(ctx, "autoupdate: application update available",
		slog.String("current", current), slog.String("latest", latest))

	if err := application.RunUpdate(ctx, log); err != nil {
		return err
	}
	log.InfoContext(ctx, "autoupdate: application update applied; restarting service")
	time.Sleep(preRestartDelay)
	application.RestartService(log)
	return nil
}

// normaliseVersion strips a leading "v" so "v1.2.3" and "1.2.3" compare equal.
func normaliseVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
