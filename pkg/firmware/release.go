package firmware

// release.go queries the pi-bmc/firmware-images GitHub releases for the
// latest u-boot image and compares versions.
//
// Tag naming convention:
//   u-boot-v2026.07-rc1
//   u-boot-v2026.07
//
// The "u-boot-" prefix identifies the artifact family. The remainder
// (v2026.07[-rcN]) is the version proper. v2026.07 is considered newer
// than v2026.07-rc1 (release > release candidate of same MAJOR.MINOR).
//
// The asset we care about is named exactly "uboot-rpi.img.xz".

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	ubootReleasesURL = "https://api.github.com/repos/pi-bmc/firmware-images/releases"
	ubootTagPrefix   = "u-boot-"
	ubootAssetName   = "uboot-rpi.img.xz"

	releaseCacheTTL = 1 * time.Hour
)

// UBootRelease describes a u-boot release available upstream.
type UBootRelease struct {
	Version  string `json:"version"`  // e.g. "v2026.07-rc1"
	Tag      string `json:"tag"`      // e.g. "u-boot-v2026.07-rc1"
	AssetURL string `json:"assetUrl"` // browser download URL for uboot-rpi.img.xz
	Size     int64  `json:"size"`
}

var (
	releaseCacheMu     sync.Mutex
	releaseCacheAll    []UBootRelease // all parsed releases; nil = not yet fetched
	releaseCacheExpiry time.Time
)

// allUBootReleases returns all u-boot releases from GitHub. Cached for 1 hour.
func allUBootReleases() ([]UBootRelease, error) {
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()

	if releaseCacheAll != nil && time.Now().Before(releaseCacheExpiry) {
		return releaseCacheAll, nil
	}

	all, err := fetchAllUBootReleases()
	if err != nil {
		return nil, err
	}
	releaseCacheAll = all
	releaseCacheExpiry = time.Now().Add(releaseCacheTTL)
	return all, nil
}

// LatestUBootRelease returns the newest u-boot release on GitHub. Cached
// for 1 hour on success; failures are not cached.
func LatestUBootRelease() (*UBootRelease, error) {
	all, err := allUBootReleases()
	if err != nil {
		return nil, err
	}
	var best *UBootRelease
	for i := range all {
		if best == nil || CompareUBootVersions(all[i].Version, best.Version) > 0 {
			r := all[i]
			best = &r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no u-boot release with asset %q found", ubootAssetName)
	}
	log.Debugf("firmware: latest u-boot release %s (%s)", best.Version, best.AssetURL)
	return best, nil
}

// ReleaseByVersion finds the GitHub release for a specific U-Boot version
// string (e.g. "v2026.04" or "2026.04"). Returns an error if not found.
func ReleaseByVersion(version string) (*UBootRelease, error) {
	all, err := allUBootReleases()
	if err != nil {
		return nil, err
	}
	norm := strings.ToLower(strings.TrimPrefix(version, "v"))
	for i := range all {
		if strings.ToLower(strings.TrimPrefix(all[i].Version, "v")) == norm {
			r := all[i]
			return &r, nil
		}
	}
	return nil, fmt.Errorf("u-boot release %q not found in GitHub releases", version)
}

// InvalidateLatestUBootCache forces the next release lookup to refetch from GitHub.
func InvalidateLatestUBootCache() {
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()
	releaseCacheAll = nil
	releaseCacheExpiry = time.Time{}
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
	Draft   bool      `json:"draft"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func fetchAllUBootReleases() ([]UBootRelease, error) {
	req, err := http.NewRequest("GET", ubootReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var ghReleases []ghRelease
	if err := json.Unmarshal(body, &ghReleases); err != nil {
		return nil, fmt.Errorf("unmarshal releases: %w", err)
	}

	var result []UBootRelease
	for i := range ghReleases {
		r := &ghReleases[i]
		if r.Draft {
			continue
		}
		if !strings.HasPrefix(r.TagName, ubootTagPrefix) {
			continue
		}
		version := strings.TrimPrefix(r.TagName, ubootTagPrefix)
		asset := findUBootAsset(r.Assets)
		if asset == nil {
			continue
		}
		result = append(result, UBootRelease{
			Version:  version,
			Tag:      r.TagName,
			AssetURL: asset.BrowserDownloadURL,
			Size:     asset.Size,
		})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no u-boot releases with asset %q found", ubootAssetName)
	}
	log.Debugf("firmware: fetched %d u-boot releases from GitHub", len(result))
	return result, nil
}

func findUBootAsset(assets []ghAsset) *ghAsset {
	for i := range assets {
		if assets[i].Name == ubootAssetName {
			return &assets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Version comparison
// ---------------------------------------------------------------------------

// versionParts holds a parsed version like "v2026.07-rc1".
//
//	numbers: [2026, 7]    (dot-separated decimal segments after stripping "v")
//	pre:     "rc1"        (anything after the first "-"; empty for releases)
//	preNum:  1            (numeric tail of pre, 0 if none)
type versionParts struct {
	numbers []int
	pre     string
	preNum  int
}

var preNumRe = regexp.MustCompile(`(\d+)$`)

func parseUBootVersion(v string) versionParts {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")

	pre := ""
	if i := strings.Index(v, "-"); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}

	var nums []int
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(strings.TrimLeft(seg, "0"))
		if err != nil || seg == "" {
			// non-numeric segment: stop parsing numbers (treat as 0).
			n = 0
		}
		nums = append(nums, n)
	}

	preNum := 0
	if pre != "" {
		if m := preNumRe.FindString(pre); m != "" {
			if n, err := strconv.Atoi(m); err == nil {
				preNum = n
			}
		}
	}
	return versionParts{numbers: nums, pre: pre, preNum: preNum}
}

// CompareUBootVersions compares two u-boot version strings (with or without
// the "u-boot-" prefix and "v" prefix). Returns -1, 0, or 1 with semantics
// a<b, a==b, a>b. A release without a "-rcN" suffix is greater than the
// same MAJOR.MINOR with one ("v2026.07" > "v2026.07-rc1").
func CompareUBootVersions(a, b string) int {
	a = strings.TrimPrefix(a, ubootTagPrefix)
	b = strings.TrimPrefix(b, ubootTagPrefix)

	pa := parseUBootVersion(a)
	pb := parseUBootVersion(b)

	// Compare numeric segments first.
	n := len(pa.numbers)
	if len(pb.numbers) > n {
		n = len(pb.numbers)
	}
	for i := 0; i < n; i++ {
		va, vb := 0, 0
		if i < len(pa.numbers) {
			va = pa.numbers[i]
		}
		if i < len(pb.numbers) {
			vb = pb.numbers[i]
		}
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}

	// Numeric segments equal: pre-release < release.
	if pa.pre == "" && pb.pre == "" {
		return 0
	}
	if pa.pre == "" {
		return 1
	}
	if pb.pre == "" {
		return -1
	}
	// Both have pre-release tags. Compare lexicographically by string,
	// then numerically by trailing number.
	if pa.pre == pb.pre {
		return 0
	}
	// Compare pre identifier prefix (e.g. "rc" vs "beta").
	preA := strings.TrimRight(pa.pre, "0123456789")
	preB := strings.TrimRight(pb.pre, "0123456789")
	if preA != preB {
		if preA < preB {
			return -1
		}
		return 1
	}
	if pa.preNum != pb.preNum {
		if pa.preNum < pb.preNum {
			return -1
		}
		return 1
	}
	return 0
}
