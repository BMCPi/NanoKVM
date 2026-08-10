// Package eepromupdater is the BMC-side analogue of rpi-eeprom-update: it
// finds and downloads the latest published pieeprom-*.bin from upstream
// (raspberrypi/rpi-eeprom on GitHub) so the BMC can stage it on the
// shared USB FAT for the host's rpi-eeprom-update to flash on next boot.
//
// We do NOT flash the EEPROM ourselves. The BMC runs out-of-band of the
// host OS and only manages the FAT volume both ends share over the USB
// gadget.
//
// Discovery uses the upstream-maintained `firmware-<plat>/versions.txt`
// index file served directly from raw.githubusercontent.com. That avoids
// api.github.com's 60 req/hr unauthenticated rate limit and the
// symlink-vs-array surprises of the Contents API (`stable` is a symlink
// to `latest`, which the API returns as an object). versions.txt is the
// same file rpi-eeprom-update consults and records the release channel
// per binary:
//
//	# version   build_epoch  fw_git_hash  release  mfg_ver
//	2026-05-22  1779408415   7dcdc4b8     latest   1
//	2026-05-11  1778498402   66f33f7e     default  1
//	...
//
// Releases: default (= recommended/stable), latest (= newest),
// old (= archived). Files live at firmware-<plat>/<release>/pieeprom-*.bin
// alongside recovery.bin, which rpi-eeprom-update needs on the boot FAT
// in order to apply a staged update at next boot.
package eepromupdater

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Platform identifies which SoC's EEPROM image directory we look in.
type Platform string

const (
	PlatformRPi5 Platform = "2712" // BCM2712 / RPi 5
	PlatformRPi4 Platform = "2711" // BCM2711 / RPi 4
)

// Channel selects an upstream release stream. Channel values are the
// stable BMC-facing names; release() maps them to the rpi-eeprom
// versions.txt release column ("default" / "latest").
type Channel string

const (
	// ChannelStable resolves to the upstream "default" release — the
	// recommended pieeprom rpi-eeprom-update would pick out of the box.
	ChannelStable Channel = "stable"
	// ChannelBeta resolves to the upstream "latest" release — the newest
	// published binary, which may include not-yet-default fixes.
	ChannelBeta Channel = "beta"
	// ChannelCritical is retained for API compatibility. Upstream no
	// longer publishes a separate critical channel; it is treated as
	// ChannelStable.
	ChannelCritical Channel = "critical"
)

// release returns the versions.txt release column value for c.
func (c Channel) release() string {
	switch c {
	case ChannelBeta:
		return "latest"
	case ChannelStable, ChannelCritical, "":
		return "default"
	default:
		return string(c)
	}
}

// Repo / branch the EEPROM binaries are published from.
const (
	upstreamOwner  = "raspberrypi"
	upstreamRepo   = "rpi-eeprom"
	upstreamBranch = "master"
	rawBaseDefault = "https://raw.githubusercontent.com"
)

// rawBase is overridable from tests; production code uses rawBaseDefault.
var rawBase = rawBaseDefault

// Image describes one published EEPROM binary on a channel.
type Image struct {
	Name     string    `json:"name"`     // e.g. "pieeprom-2024-12-04.bin"
	URL      string    `json:"url"`      // raw GitHub download URL
	Size     int64     `json:"size"`     // bytes (0 if not probed)
	Version  string    `json:"version"`  // "2024-12-04" parsed from versions.txt
	Platform Platform  `json:"platform"` // 2712 or 2711
	Channel  Channel   `json:"channel"`  // stable/beta
	FoundAt  time.Time `json:"foundAt"`  // when the lookup happened
	// RecoveryURL points at the recovery.bin shipped in the same channel
	// directory. rpi-eeprom-update needs recovery.bin sitting next to
	// pieeprom.upd on the boot FAT in order to actually apply a staged
	// update on next boot.
	RecoveryURL  string `json:"recoveryUrl,omitempty"`
	RecoverySize int64  `json:"recoverySize,omitempty"`
}

// FindLatestOptions tunes FindLatest. Zero-value defaults are safe (RPi 5
// stable, default HTTP client + 15s timeout).
type FindLatestOptions struct {
	Platform   Platform
	Channel    Channel
	HTTPClient *http.Client
}

// FindLatest fetches firmware-<plat>/versions.txt and returns the newest
// pieeprom matching the requested channel's release.
//
// Network: one GET to raw.githubusercontent.com. No auth, no API rate
// limit. The file is small (a few KB) and cache-friendly.
func FindLatest(ctx context.Context, opts FindLatestOptions) (*Image, error) {
	platform := opts.Platform
	if platform == "" {
		platform = PlatformRPi5
	}
	channel := opts.Channel
	if channel == "" {
		channel = ChannelStable
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	indexURL := fmt.Sprintf("%s/%s/%s/%s/firmware-%s/versions.txt",
		rawBase, upstreamOwner, upstreamRepo, upstreamBranch, platform)
	data, err := httpGet(ctx, client, indexURL, "versions.txt", 1*1024*1024)
	if err != nil {
		return nil, err
	}

	wantRelease := channel.release()
	version, err := pickNewestVersion(data, wantRelease)
	if err != nil {
		return nil, fmt.Errorf("find latest: %w", err)
	}

	dirURL := fmt.Sprintf("%s/%s/%s/%s/firmware-%s/%s",
		rawBase, upstreamOwner, upstreamRepo, upstreamBranch, platform, wantRelease)
	name := fmt.Sprintf("pieeprom-%s.bin", version)
	return &Image{
		Name:        name,
		URL:         dirURL + "/" + name,
		Version:     version,
		Platform:    platform,
		Channel:     channel,
		FoundAt:     time.Now(),
		RecoveryURL: dirURL + "/recovery.bin",
	}, nil
}

// pickNewestVersion scans versions.txt content for the newest entry whose
// release column equals wantRelease. versions.txt is sorted newest-first,
// so the first match wins. Comment / blank lines are skipped.
func pickNewestVersion(data []byte, wantRelease string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Columns: version  build_epoch  fw_git_hash  release  [mfg_ver]
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[3] != wantRelease {
			continue
		}
		return fields[0], nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("scan versions.txt: %w", err)
	}
	return "", fmt.Errorf("no entries with release=%q", wantRelease)
}

// Download fetches img.URL and returns the bytes. Verifies the response
// size matches img.Size when img.Size > 0.
func Download(ctx context.Context, img *Image, client *http.Client) ([]byte, error) {
	if img == nil || img.URL == "" {
		return nil, errors.New("nil image or empty URL")
	}
	return fetchSized(ctx, client, img.URL, img.Name, img.Size)
}

// DownloadRecovery fetches the recovery.bin associated with img.
func DownloadRecovery(ctx context.Context, img *Image, client *http.Client) ([]byte, error) {
	if img == nil || img.RecoveryURL == "" {
		return nil, errors.New("nil image or empty recovery URL")
	}
	return fetchSized(ctx, client, img.RecoveryURL, "recovery.bin", img.RecoverySize)
}

// fetchSized GETs url and verifies the body length matches expectedSize
// when non-zero. Capped at the EEPROM-image safety limit.
func fetchSized(ctx context.Context, client *http.Client, url, name string, expectedSize int64) ([]byte, error) {
	// EEPROM binaries are ~2 MB and recovery.bin is similar; cap at 4 MB.
	const maxBytes = 4 * 1024 * 1024
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	body, err := httpGet(ctx, client, url, name, maxBytes)
	if err != nil {
		return nil, err
	}
	if expectedSize > 0 && int64(len(body)) != expectedSize {
		return nil, fmt.Errorf("%s size mismatch: got %d, expected %d", name, len(body), expectedSize)
	}
	return body, nil
}

// httpGet performs a GET with a context, returns the body up to maxBytes,
// and errors if the response is non-200 or exceeds the cap.
func httpGet(ctx context.Context, client *http.Client, url, name string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s exceeded max size %d bytes", name, maxBytes)
	}
	return body, nil
}
