package redfish

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// The RHI-only host model, after the JetKVM pattern: the BMC never touches
// host storage. A boot override is BMC state that the host's firmware — a
// Redfish client on the USB host interface — reads and applies itself at
// boot. Everything the BMC "knows" about the host (identity, boot options,
// memory, drives, BIOS attributes) is what the host last reported over that
// interface; nothing is scraped out of an EEPROM.
//
// Two ownership domains, two write directions:
//
//   - Operator (LAN, authenticated) stages Boot overrides and pending BIOS
//     settings. These are instructions the host has not read yet, so they
//     persist immediately.
//   - Host firmware (host interface, unauthenticated per DSP0270) reports
//     identity, boot progress, boot options, memory, drives, attributes.
//     These persist debounced: a booting host POSTs dozens of resources in
//     seconds, and each one need not be its own flash write.

// HostBootOverride is the operator-staged override the host firmware
// consumes on its next boot.
type HostBootOverride struct {
	Target  string `json:"target"`
	Enabled string `json:"enabled"`
}

// HostReport is everything the managed host has said about itself.
type HostReport struct {
	BiosVersion  string    `json:"bios_version,omitempty"`
	Manufacturer string    `json:"manufacturer,omitempty"`
	Model        string    `json:"model,omitempty"`
	SerialNumber string    `json:"serial_number,omitempty"`
	UUID         string    `json:"uuid,omitempty"`
	BootProgress string    `json:"boot_progress,omitempty"`
	ReportedAt   time.Time `json:"reported_at,omitzero"`
}

// hostState is the whole persisted document. CapturedAt lets a reader judge
// how far behind the running host a restored copy may be.
type hostState struct {
	mu sync.RWMutex

	CapturedAt time.Time        `json:"captured_at"`
	Boot       HostBootOverride `json:"boot"`
	Host       HostReport       `json:"host"`

	// Host-owned collections, keyed by Id. The host re-POSTs them whenever
	// its BDS re-enumerates; an operator only reads.
	BootOptions map[string]map[string]any `json:"boot_options"`
	Memory      map[string]map[string]any `json:"memory"`
	Drives      map[string]map[string]any `json:"drives"`

	// BiosAttributes is what is in effect now (host-reported); BiosPending
	// is what an operator staged for the next boot (the @Redfish.Settings
	// pattern). BiosRegistry stays nil until the host PUTs one: "no
	// attributes" and "not reported yet" are different answers.
	BiosAttributes map[string]any `json:"bios_attributes"`
	BiosPending    map[string]any `json:"bios_pending"`
	BiosRegistry   map[string]any `json:"bios_registry,omitempty"`

	SecureBoot map[string]any `json:"secure_boot"`

	// Thermal is the chassis thermal report (fans, temperatures) the host's
	// firmware PATCHes during boot; nil until it has.
	Thermal map[string]any `json:"thermal,omitempty"`
}

var host = &hostState{
	Boot:           HostBootOverride{Target: "None", Enabled: "Disabled"},
	BootOptions:    map[string]map[string]any{},
	Memory:         map[string]map[string]any{},
	Drives:         map[string]map[string]any{},
	BiosAttributes: map[string]any{},
	BiosPending:    map[string]any{},
	SecureBoot: map[string]any{
		"SecureBootEnable":             false,
		"SecureBootCurrentBoot":        "Disabled",
		"SecureBootMode":               "SetupMode",
		"@Redfish.WriteableProperties": []string{"SecureBootEnable"},
	},
}

// hostStatePath lives on the persistent partition next to the rest of the
// app's state. Overridable for tests.
var hostStatePath = "/var/lib/nanokvm/bmc_state.json"

// hostStateSaveDelay coalesces the write burst of a booting host.
const hostStateSaveDelay = 2 * time.Second

var (
	hostSaveMu    sync.Mutex
	hostSaveTimer *time.Timer
)

// IsHostInterfaceRequest reports whether a request arrived over the USB
// host-interface link (DSP0270). The subnet is the RHI address the network
// manager assigns to usb0; requests from it are the managed host's firmware.
// The nftables isolation in pkg/network guarantees 169.254/16 traffic cannot
// arrive via eth0, so the source address is a real trust boundary here.
func IsHostInterfaceRequest(c *gin.Context) bool {
	ip := net.ParseIP(c.ClientIP())
	if ip == nil {
		return false
	}
	cidr := config.GetInstance().Network.RHI.Address
	if cidr == "" {
		return false
	}
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return subnet.Contains(ip)
}

// hostWritable rejects writes to host-owned resources from anywhere but the
// host interface. Without it a LAN client could claim the host has hardware
// it does not have, and the BMC would repeat that back as fact.
func hostWritable(c *gin.Context) bool {
	if IsHostInterfaceRequest(c) {
		return true
	}
	redfishErrorResponse(c, http.StatusForbidden,
		"this resource is reported by the managed host and is writable only over the host interface")
	return false
}

// --- boot override -----------------------------------------------------------

// BootOverride returns the staged override.
func BootOverride() HostBootOverride {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return host.Boot
}

// SetBootOverride stages a boot override and persists it immediately: it is
// an operator instruction the host has not read yet, and must not be lost to
// a debounce window if power drops.
func SetBootOverride(target, enabled string) {
	host.mu.Lock()
	host.Boot = HostBootOverride{Target: target, Enabled: enabled}
	host.mu.Unlock()
	hostStateFlush()
}

// --- host reports ------------------------------------------------------------

// updateHostReported merges a host identity/progress report and schedules a
// debounced save.
func updateHostReported(update func(*HostReport)) {
	host.mu.Lock()
	update(&host.Host)
	host.Host.ReportedAt = time.Now()
	host.mu.Unlock()
	hostStateSave()
}

// HostReported returns a snapshot of what the host last said about itself.
func HostReported() (HostReport, HostBootOverride) {
	host.mu.RLock()
	defer host.mu.RUnlock()
	return host.Host, host.Boot
}

// --- persistence -------------------------------------------------------------

func hostStateSave() {
	hostSaveMu.Lock()
	defer hostSaveMu.Unlock()
	if hostSaveTimer != nil {
		hostSaveTimer.Stop()
	}
	hostSaveTimer = time.AfterFunc(hostStateSaveDelay, func() {
		if err := hostStateWrite(); err != nil {
			log.Warnf("persisting BMC host state: %v", err)
		}
	})
}

// FlushHostState writes any debounced host state to disk now. Called on the
// shutdown path: the host's own reports save debounced, so without this a
// SIGTERM inside the debounce window discards whatever the host last said and
// the BMC comes back advertising the previous boot's inventory.
func FlushHostState() { hostStateFlush() }

// hostStateFlush writes immediately, for operator instructions and shutdown.
func hostStateFlush() {
	hostSaveMu.Lock()
	if hostSaveTimer != nil {
		hostSaveTimer.Stop()
		hostSaveTimer = nil
	}
	hostSaveMu.Unlock()
	if err := hostStateWrite(); err != nil {
		log.Warnf("flushing BMC host state: %v", err)
	}
}

// hostStateWrite serialises atomically: temp file plus rename, because this
// file is read at boot and a torn write would cost the data it protects.
func hostStateWrite() error {
	host.mu.RLock()
	host.CapturedAt = time.Now()
	encoded, err := json.MarshalIndent(host, "", "  ")
	host.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	dir := filepath.Dir(hostStatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".bmc_state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	return os.Rename(tmpName, hostStatePath)
}

// LoadHostState restores the last snapshot at startup, before any route can
// serve. A corrupt file is not worth failing startup over; the host rewrites
// everything on its next boot.
func LoadHostState() {
	data, err := os.ReadFile(hostStatePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("reading persisted BMC host state: %v", err)
		}
		return
	}

	restored := &hostState{}
	if err := json.Unmarshal(data, restored); err != nil {
		log.Warnf("persisted BMC host state is unreadable; ignoring it: %v", err)
		return
	}

	host.mu.Lock()
	if restored.Boot.Target != "" && restored.Boot.Enabled != "" {
		host.Boot = restored.Boot
	}
	host.Host = restored.Host
	for dst, src := range map[*map[string]map[string]any]map[string]map[string]any{
		&host.BootOptions: restored.BootOptions,
		&host.Memory:      restored.Memory,
		&host.Drives:      restored.Drives,
	} {
		if src != nil {
			*dst = src
		}
	}
	if restored.BiosAttributes != nil {
		host.BiosAttributes = restored.BiosAttributes
	}
	if restored.BiosPending != nil {
		host.BiosPending = restored.BiosPending
	}
	host.BiosRegistry = restored.BiosRegistry
	if restored.SecureBoot != nil {
		host.SecureBoot = restored.SecureBoot
	}
	host.Thermal = restored.Thermal

	// Collapse duplicate members accumulated by earlier builds that minted a
	// fresh id for every keyless re-POST (one ghost per host boot). Lowest
	// id wins so references stay as stable as they can be.
	for _, m := range []map[string]map[string]any{host.BootOptions, host.Memory, host.Drives} {
		dedupeHostCollection(m)
	}
	captured := restored.CapturedAt
	host.mu.Unlock()

	age := "unknown"
	if !captured.IsZero() {
		age = time.Since(captured).Round(time.Second).String()
	}
	log.Infof("restored BMC host state captured %s ago; the host overwrites it on its next boot", age)
}

// dedupeHostCollection removes members whose content is identical to another
// member with a lexicographically smaller id.
func dedupeHostCollection(m map[string]map[string]any) {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		for _, keep := range ids[:i] {
			if _, kept := m[keep]; kept && reflect.DeepEqual(m[keep], m[id]) {
				delete(m, id)
				break
			}
		}
	}
}

// --- ETag helpers ------------------------------------------------------------
//
// EDK2's RedfishETagDxe round-trips ETags as If-Match on writes; a service
// without them makes that protection vacuous. Derived from content: stable
// when nothing changed, different when anything did.

func hostETag(body any) string {
	encoded, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("W/\"%x\"", sha256.Sum256(encoded))
}

func hostETagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	strip := func(s string) string {
		return strings.Trim(strings.TrimPrefix(strings.TrimSpace(s), "W/"), `"`)
	}
	want := strip(etag)
	for _, candidate := range strings.Split(header, ",") {
		if strip(candidate) == want {
			return true
		}
	}
	return false
}

// hostCheckIfMatch enforces a conditional write; on mismatch it writes the
// 412 and the caller simply returns.
func hostCheckIfMatch(c *gin.Context, current any) bool {
	match := c.GetHeader("If-Match")
	if match == "" {
		return true
	}
	if hostETagMatches(match, hostETag(current)) {
		return true
	}
	redfishErrorResponse(c, http.StatusPreconditionFailed, "ETag does not match the current resource")
	return false
}

func copyAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
