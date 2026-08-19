package application

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// latestCacheTTL controls how long a successful GitHub release lookup is
// reused before re-querying the API.
const latestCacheTTL = 1 * time.Hour

var (
	latestCacheMu     sync.Mutex
	latestCacheValue  *Latest
	latestCacheExpiry time.Time
)

// githubRelease is the subset of GitHub's release API response we need.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Latest holds the resolved release metadata for an update.
type Latest struct {
	Version string
	Name    string
	Url     string
	Size    int64
}

// Version is the build-time version stamped via goreleaser ldflags
// (propagated from main); "dev" when built without stamping.
var Version = "dev"

// CurrentVersion returns the running application version: the build-time
// Version when stamped, else the version file of the active install dir.
func CurrentVersion() string {
	if Version != "dev" && Version != "" {
		return Version
	}

	// Read the version file of the install the init script launches (the
	// self-update dir, its rollback, or the baked-in image copy).
	versionFile := fmt.Sprintf("%s/version", ActiveAppDir())
	if data, err := os.ReadFile(versionFile); err == nil {
		v := strings.TrimSpace(string(data))
		if v != "" {
			return v
		}
	}

	return Version
}

func getLatest(ctx context.Context) (*Latest, error) {
	latestCacheMu.Lock()
	defer latestCacheMu.Unlock()

	if latestCacheValue != nil && time.Now().Before(latestCacheExpiry) {
		log.Debugf("latest release: cache hit (%s)", latestCacheValue.Version)
		return latestCacheValue, nil
	}

	latest, err := fetchLatest(ctx)
	if err != nil {
		return nil, err
	}

	latestCacheValue = latest
	latestCacheExpiry = time.Now().Add(latestCacheTTL)
	return latest, nil
}

func fetchLatest(ctx context.Context) (*Latest, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", GitHubOwner, GitHubRepo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("failed to request latest release: %v", err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Debugf("github responded with status code: %d", resp.StatusCode)
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("unmarshal release: %w", err)
	}

	asset := findPlatformAsset(release.Assets)
	if asset == nil {
		return nil, fmt.Errorf("no matching asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	version := strings.TrimPrefix(release.TagName, "v")

	log.Debugf("latest release: %s (%s)", version, asset.Name)
	return &Latest{
		Version: version,
		Name:    asset.Name,
		Url:     asset.BrowserDownloadURL,
		Size:    asset.Size,
	}, nil
}

// findPlatformAsset returns the archive asset matching the current OS/arch.
func findPlatformAsset(assets []githubAsset) *githubAsset {
	suffix := fmt.Sprintf("_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for i := range assets {
		if strings.HasSuffix(assets[i].Name, suffix) {
			return &assets[i]
		}
	}
	return nil
}
