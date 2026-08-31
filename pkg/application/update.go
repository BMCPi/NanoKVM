package application

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

const (
	maxTries = 3
)

// RestartDelay is the wait after a successful update response before the
// service actually restarts, giving the response time to flush.
const RestartDelay = 1 * time.Second

// RunUpdate downloads and installs the latest application release. Acquires
// the global update lock so concurrent runs (HTTP trigger + auto-update
// ticker) can't collide. On success the service restart is initiated by the
// caller (HTTP handler) or by the auto-update service after a short delay.
//
// ctx bounds the release lookup and the download. Callers pass a context
// detached from the HTTP request: an update replaces the running binary, and
// abandoning that partway because a browser tab closed would leave the install
// half-applied.
func RunUpdate(ctx context.Context, log *slog.Logger) error {
	if !acquireUpdateLock() {
		return fmt.Errorf("update already in progress")
	}
	defer releaseUpdateLock()
	return update(ctx, log)
}

// RunOfflineUpdate installs an update package supplied by the caller: it
// acquires the update lock, prepares CacheDir, invokes stage to place the
// package there (returning its path), and installs it. stage runs inside
// the lock so an upload cannot race a concurrent update, and the lock and
// cache lifecycle stay owned by this package.
func RunOfflineUpdate(log *slog.Logger, stage func(cacheDir string) (string, error)) error {
	if !acquireUpdateLock() {
		return fmt.Errorf("update already in progress")
	}
	defer releaseUpdateLock()

	_ = os.RemoveAll(CacheDir)
	_ = os.MkdirAll(CacheDir, 0o755)
	defer func() {
		_ = os.RemoveAll(CacheDir)
	}()

	target, err := stage(CacheDir)
	if err != nil {
		return err
	}
	return installPackage(log, target)
}

// RestartService restarts the server by exiting: busybox init runs the
// server under an inittab ::respawn entry, so a clean exit is a restart,
// and the respawned launcher re-walks the app -> app.prev -> /kvmapp
// cascade, picking up whatever an update just installed. Raising SIGTERM
// at ourselves (rather than calling os.Exit) routes through main's signal
// handler, so listeners and the gadget shut down exactly as on a system
// stop.
func RestartService(log *slog.Logger) {
	log.Info("restart requested; exiting for init to respawn")
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// RestartAfter waits delay, or until doneCtx is done, whichever comes first,
// then restarts the service. It is the shared tail of an update handler's
// choreography: write the success response, then call this — either
// synchronously, so the response finishes writing before the handler
// returns, or in its own goroutine, so the handler can return immediately
// after writing. doneCtx is watched instead of a bare Sleep: at shutdown
// there is no point restarting a service the process is already tearing
// down, and a bare Sleep would hold the caller past that point for no
// benefit.
func RestartAfter(doneCtx context.Context, delay time.Duration, log *slog.Logger) {
	select {
	case <-time.After(delay):
	case <-doneCtx.Done():
		return
	}
	RestartService(log)
}

// LatestVersion returns the latest available release version string (or
// empty when the lookup fails). Exposed so the auto-update service can
// compare against the running version without depending on internals.
func LatestVersion(ctx context.Context, log *slog.Logger) string {
	l, err := getLatest(ctx, log)
	if err != nil || l == nil {
		return ""
	}
	return l.Version
}

func update(ctx context.Context, log *slog.Logger) error {
	_ = os.RemoveAll(CacheDir)
	_ = os.MkdirAll(CacheDir, 0o755)
	defer func() {
		_ = os.RemoveAll(CacheDir)
	}()

	latest, err := getLatest(ctx, log)
	if err != nil {
		return err
	}

	target := fmt.Sprintf("%s/%s", CacheDir, latest.Name)
	if err := download(ctx, log, latest.URL, target); err != nil {
		log.ErrorContext(ctx, "download app failed", slog.Any("err", err))
		return err
	}

	if err := installPackage(log, target); err != nil {
		log.ErrorContext(ctx, "failed to install package", slog.Any("err", err))
		return err
	}

	return nil
}

func download(ctx context.Context, log *slog.Logger, url string, target string) (err error) {
	for i := range maxTries {
		log.DebugContext(ctx, "download attempt", slog.Int("attempt", i+1), slog.Int("max", maxTries))
		if i > 0 {
			// Back off between attempts, but give up immediately if the caller
			// has gone away rather than sleeping through a cancelled update.
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.ErrorContext(ctx, "new request err", slog.Any("err", err))
			continue
		}

		log.DebugContext(ctx, "update will be saved", slog.String("path", target))
		err = utils.Download(req, target, log)
		if err != nil {
			log.ErrorContext(ctx, "downloading latest application failed, try again", slog.Any("err", err))
			continue
		}
		return nil
	}
	return err
}
