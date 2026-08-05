package application

import (
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/server/proto"
	"github.com/pi-bmc/nanokvm-app/server/utils"
)

const (
	maxTries = 3
)

func (s *Service) Update(c *gin.Context) {
	var rsp proto.Response

	if !acquireUpdateLock() {
		rsp.ErrRsp(c, -1, "update already in progress")
		return
	}
	defer releaseUpdateLock()

	if err := update(); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("update failed: %s", err))
		return
	}

	rsp.OkRsp(c)
	log.Debugf("update application success")

	time.Sleep(1 * time.Second)
	RestartService()
}

// RunUpdate downloads and installs the latest application release without an
// HTTP context. Acquires the global update lock so concurrent runs (HTTP
// trigger + auto-update ticker) can't collide. On success the service
// restart is initiated by the caller (HTTP handler) or by the auto-update
// service after a short delay.
func RunUpdate() error {
	if !acquireUpdateLock() {
		return fmt.Errorf("update already in progress")
	}
	defer releaseUpdateLock()
	return update()
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
func LatestVersion() string {
	l, err := getLatest()
	if err != nil || l == nil {
		return ""
	}
	return l.Version
}

// CurrentVersion returns the running application version.
func CurrentVersion() string {
	return currentAppVersion()
}

func update() error {
	_ = os.RemoveAll(CacheDir)
	_ = os.MkdirAll(CacheDir, 0o755)
	defer func() {
		_ = os.RemoveAll(CacheDir)
	}()

	latest, err := getLatest()
	if err != nil {
		return err
	}

	target := fmt.Sprintf("%s/%s", CacheDir, latest.Name)
	if err := download(latest.Url, target); err != nil {
		log.Errorf("download app failed: %s", err)
		return err
	}

	if err := installPackage(target); err != nil {
		log.Errorf("failed to install package: %v", err)
		return err
	}

	return nil
}

func download(url string, target string) (err error) {
	for i := range maxTries {
		log.Debugf("attempt #%d/%d", i+1, maxTries)
		if i > 0 {
			time.Sleep(time.Second * 3)
		}

		var req *http.Request
		req, err = http.NewRequest("GET", url, nil)
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
