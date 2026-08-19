package application

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/utils"
)

const (
	maxTries = 3
)

// RunUpdate downloads and installs the latest application release. Acquires
// the global update lock so concurrent runs (HTTP trigger + auto-update
// ticker) can't collide. On success the service restart is initiated by the
// caller (HTTP handler) or by the auto-update service after a short delay.
//
// ctx bounds the release lookup and the download. Callers pass a context
// detached from the HTTP request: an update replaces the running binary, and
// abandoning that partway because a browser tab closed would leave the install
// half-applied.
func RunUpdate(ctx context.Context) error {
	if !acquireUpdateLock() {
		return fmt.Errorf("update already in progress")
	}
	defer releaseUpdateLock()
	return update(ctx)
}

// RunOfflineUpdate installs an update package supplied by the caller: it
// acquires the update lock, prepares CacheDir, invokes stage to place the
// package there (returning its path), and installs it. stage runs inside
// the lock so an upload cannot race a concurrent update, and the lock and
// cache lifecycle stay owned by this package.
func RunOfflineUpdate(stage func(cacheDir string) (string, error)) error {
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
	return installPackage(target)
}

// RestartService restarts the server by exiting: busybox init runs the
// server under an inittab ::respawn entry, so a clean exit is a restart,
// and the respawned launcher re-walks the app -> app.prev -> /kvmapp
// cascade, picking up whatever an update just installed. Raising SIGTERM
// at ourselves (rather than calling os.Exit) routes through main's signal
// handler, so listeners and the gadget shut down exactly as on a system
// stop.
func RestartService() {
	log.Info("restart requested; exiting for init to respawn")
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// LatestVersion returns the latest available release version string (or
// empty when the lookup fails). Exposed so the auto-update service can
// compare against the running version without depending on internals.
func LatestVersion(ctx context.Context) string {
	l, err := getLatest(ctx)
	if err != nil || l == nil {
		return ""
	}
	return l.Version
}

func update(ctx context.Context) error {
	_ = os.RemoveAll(CacheDir)
	_ = os.MkdirAll(CacheDir, 0o755)
	defer func() {
		_ = os.RemoveAll(CacheDir)
	}()

	latest, err := getLatest(ctx)
	if err != nil {
		return err
	}

	target := fmt.Sprintf("%s/%s", CacheDir, latest.Name)
	if err := download(ctx, latest.Url, target); err != nil {
		log.Errorf("download app failed: %s", err)
		return err
	}

	if err := installPackage(target); err != nil {
		log.Errorf("failed to install package: %v", err)
		return err
	}

	return nil
}

func download(ctx context.Context, url string, target string) (err error) {
	for i := range maxTries {
		log.Debugf("attempt #%d/%d", i+1, maxTries)
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
			log.Errorf("new request err: %s", err)
			continue
		}

		log.Debugf("update will be saved to: %s", target)
		err = utils.Download(req, target)
		if err != nil {
			log.Errorf("downloading latest application failed, try again...")
			continue
		}
		return nil
	}
	return err
}
