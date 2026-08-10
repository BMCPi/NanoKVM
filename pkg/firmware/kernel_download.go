package firmware

import (
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
)

// Sentinel errors for StartKernelDownload callers that map errors onto
// HTTP statuses (the JSON API and the ui fragments).
var (
	ErrUnknownKernel = errors.New("unknown kernel version")
	ErrDownloadBusy  = errors.New("download already in progress")
)

// StartKernelDownload begins caching the U-Boot image mapped to kernel.
// force deletes any cached copy first, and when the forced version is the
// currently-active one the fresh image is re-activated after the download —
// "refresh" would otherwise leave the active boot image untouched, which is
// surprising when the user refreshes the version they are running.
//
// reactivating reports that second behavior so callers can phrase their
// response. The download itself runs detached from the caller: it must
// survive the request that started it.
func (c *Controller) StartKernelDownload(kernel string, force bool) (reactivating bool, err error) {
	ubootVer, ok := KernelUBootMap[kernel]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownKernel, kernel)
	}
	if c.IsDownloading() {
		return false, ErrDownloadBusy
	}
	rel, err := ReleaseByVersion(ubootVer)
	if err != nil {
		return false, err
	}

	// Snapshot active version BEFORE the download so the auto-reactivate
	// decision isn't affected by a concurrent activation racing this one.
	wasActive := force && c.ActiveUBootVersion() == ubootVer

	// DELIBERATELY DETACHED: runs past the initiating request's lifetime.
	go func(ver, url string, force, reactivate bool) {
		if force {
			c.DeleteVersionedImage(ver)
		}
		if err := c.DownloadVersionedImage(ver, url); err != nil {
			log.Errorf("versioned image download failed (%s): %v", ver, err)
			return
		}
		if reactivate {
			if err := c.ActivateVersionedImage(ver); err != nil {
				log.Errorf("auto-reactivate after refresh failed (%s): %v", ver, err)
			} else {
				log.Infof("auto-reactivated %s after refresh of currently-active image", ver)
			}
		}
	}(ubootVer, rel.AssetURL, force, wasActive)

	return wasActive, nil
}
