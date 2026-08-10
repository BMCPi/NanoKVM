// Package eepromkeys is the typed catalog of Raspberry Pi bootloader
// EEPROM configuration keys (name, type, per-platform default, description),
// transcribed from the official documentation at
// raspberrypi/documentation/.../eeprom-bootloader.adoc.
//
// The catalog has two uses inside the BMC:
//
//  1. Display — UIs can render each row alongside its documented default
//     so an operator sees what they'd be changing FROM.
//  2. Write-side default filtering — when staging a new bootconf.txt we
//     drop lines in the [all] section whose value matches the platform
//     default. Conditional sections ([gpio4=1], [HDMI=…]) are left
//     intact because the section predicate changes the meaning of the
//     value, and the rpi bootloader's own defaults don't apply there.
package eepromkeys

import (
	"sort"
	"strconv"
	"strings"
)

// Platform mirrors eepromupdater.Platform — duplicated here to avoid a
// cycle, since this package is consumed by eeprom.go (which uses both).
type Platform string

const (
	PlatformRPi5 Platform = "2712"
	PlatformRPi4 Platform = "2711"
)

// Type is the displayed/parsed type of an EEPROM key's value.
type Type string

const (
	TypeInt    Type = "integer" // signed integer, decimal
	TypeHex    Type = "hex"     // unsigned integer, hex (BOOT_ORDER style)
	TypeBool   Type = "bool"    // 0/1
	TypeString Type = "string"  // free-form text
)

// Key is one bootloader EEPROM configuration property.
type Key struct {
	Name        string `json:"name"`
	Type        Type   `json:"type"`
	Default     string `json:"default"`
	Description string `json:"description"`
	// Platforms where this key is documented as applying. If empty, key
	// is universal. Used to filter the catalog per board.
	OnlyPlatforms []Platform `json:"onlyPlatforms,omitempty"`
	// Platforms where this key is explicitly *not* applicable.
	NotPlatforms []Platform `json:"notPlatforms,omitempty"`
}

// AppliesTo reports whether the key is documented to apply on platform p.
func (k Key) AppliesTo(p Platform) bool {
	for _, x := range k.NotPlatforms {
		if x == p {
			return false
		}
	}
	if len(k.OnlyPlatforms) == 0 {
		return true
	}
	for _, x := range k.OnlyPlatforms {
		if x == p {
			return true
		}
	}
	return false
}

// catalog is the master table. Defaults reflect Pi 5 (BCM2712); per-key
// notes via OnlyPlatforms / NotPlatforms cover Pi 4 / CM exclusions.
//
// Source: raspberrypi/documentation/computers/raspberry-pi/eeprom-bootloader.adoc
var catalog = []Key{
	// --- UART / debug ---
	{
		Name: "BOOT_UART", Type: TypeBool, Default: "0",
		Description: "Enable UART debug output on GPIO 14 and 15 at 115200bps.",
	},
	{
		Name: "UART_BAUD", Type: TypeInt, Default: "115200",
		Description:   "Bootloader UART baud rate (9600–921600).",
		OnlyPlatforms: []Platform{PlatformRPi5},
	},

	// --- Power button / halt ---
	{
		Name: "WAKE_ON_GPIO", Type: TypeBool, Default: "1",
		Description:  "Low-power mode until GPIO3 or GLOBAL_EN is shorted to ground.",
		NotPlatforms: []Platform{PlatformRPi5},
	},
	{
		Name: "POWER_OFF_ON_HALT", Type: TypeBool, Default: "0",
		Description: "Switch off PMIC outputs on halt (combine with WAKE_ON_GPIO=0).",
	},
	{
		Name: "WAIT_FOR_POWER_BUTTON", Type: TypeBool, Default: "0",
		Description:   "Power off and wait for button press after first boot.",
		OnlyPlatforms: []Platform{PlatformRPi5},
	},

	// --- Boot order / watchdog ---
	{
		Name: "BOOT_ORDER", Type: TypeHex, Default: "0xf41",
		Description: "Boot-mode priority sequence; read right-to-left, up to eight modes.",
	},
	{
		Name: "BOOT_WATCHDOG_TIMEOUT", Type: TypeInt, Default: "0",
		Description: "Hardware watchdog timeout in seconds; resets if OS hasn't started in time.",
	},
	{
		Name: "BOOT_WATCHDOG_PARTITION", Type: TypeInt, Default: "0",
		Description: "Partition number to boot after a watchdog reset (A/B failover).",
	},
	{
		Name: "MAX_RESTARTS", Type: TypeInt, Default: "-1",
		Description: "Maximum RESTART-mode iterations before watchdog triggers a reset.",
	},
	{
		Name: "REBOOT_ON_FATAL_ERROR", Type: TypeBool, Default: "1",
		Description: "Display error pattern three times, then reset, on fatal boot errors.",
	},
	{
		Name: "PARTITION", Type: TypeInt, Default: "0",
		Description: "Boot partition number.",
	},
	{
		Name: "PARTITION_WALK", Type: TypeBool, Default: "0",
		Description: "Search bootable partitions if the requested one is unavailable.",
	},

	// --- SD card boot ---
	{
		Name: "SD_BOOT_MAX_RETRIES", Type: TypeInt, Default: "0",
		Description: "SD boot retry attempts before advancing to the next boot mode.",
	},
	{
		Name: "SD_OVERCURRENT_CHECK", Type: TypeBool, Default: "1",
		Description: "Check SD power overcurrent signal.",
	},
	{
		Name: "SD_QUIRKS", Type: TypeHex, Default: "0",
		Description: "Hardware workaround flags (0x1 disables SD High Speed modes).",
	},

	// --- Network boot ---
	{
		Name: "NET_BOOT_MAX_RETRIES", Type: TypeInt, Default: "0",
		Description: "Network boot retry attempts before advancing to the next boot mode.",
	},
	{
		Name: "DHCP_TIMEOUT", Type: TypeInt, Default: "45000",
		Description: "Total DHCP sequence timeout in milliseconds (min 5000).",
	},
	{
		Name: "DHCP_REQ_TIMEOUT", Type: TypeInt, Default: "4000",
		Description: "DHCP DISCOVER/REQ retry timeout in milliseconds (min 500).",
	},
	{
		Name: "TFTP_FILE_TIMEOUT", Type: TypeInt, Default: "30000",
		Description: "Individual TFTP file download timeout in milliseconds (min 5000).",
	},
	{
		Name: "TFTP_IP", Type: TypeString, Default: "",
		Description: "Optional TFTP server IP; overrides the DHCP server-ip.",
	},
	{
		Name: "TFTP_PREFIX", Type: TypeInt, Default: "0",
		Description: "Directory prefix mode: 0=serial, 1=custom string, 2=MAC address.",
	},
	{
		Name: "TFTP_PREFIX_STR", Type: TypeString, Default: "",
		Description: "Custom TFTP directory prefix when TFTP_PREFIX=1 (max 32 chars).",
	},
	{
		Name: "PXE_OPTION43", Type: TypeString, Default: "Raspberry Pi Boot",
		Description: "Custom PXE Option43 match string.",
	},
	{
		Name: "DHCP_OPTION97", Type: TypeHex, Default: "0x34695052",
		Description: "Client GUID prefix; 0 reverts to old serial-based format.",
	},
	{
		Name: "MAC_ADDRESS", Type: TypeString, Default: "",
		Description: "Override Ethernet MAC address (format dc:a6:32:01:36:c2).",
	},
	{
		Name: "MAC_ADDRESS_OTP", Type: TypeString, Default: "",
		Description: "MAC address from Customer OTP registers (format \"0,1\").",
	},
	{
		Name: "CLIENT_IP", Type: TypeString, Default: "",
		Description: "Static IP for client (skips DHCP if set together with TFTP_IP).",
	},
	{
		Name: "SUBNET", Type: TypeString, Default: "",
		Description: "Subnet mask for static IP configuration.",
	},
	{
		Name: "GATEWAY", Type: TypeString, Default: "",
		Description: "Gateway for static IP on a different subnet.",
	},
	{
		Name: "NETCONSOLE", Type: TypeString, Default: "",
		Description: "Advanced logging via network (src_port@src_ip/dev,dst_port@dst_ip/mac).",
	},

	// --- HDMI diagnostics ---
	{
		Name: "DISABLE_HDMI", Type: TypeBool, Default: "0",
		Description: "Disable HDMI boot diagnostics display when set to 1.",
	},
	{
		Name: "HDMI_DELAY", Type: TypeInt, Default: "5",
		Description: "Skip diagnostics rendering for N seconds (unless fatal error).",
	},

	// --- Self-update ---
	{
		Name: "ENABLE_SELF_UPDATE", Type: TypeBool, Default: "1",
		Description: "Allow bootloader to update itself from TFTP or USB-MSD.",
	},
	{
		Name: "FREEZE_VERSION", Type: TypeBool, Default: "0",
		Description: "Stop automatic updates; requires SD recovery to disable.",
	},

	// --- Network install / HTTP boot ---
	{
		Name: "HTTP_HOST", Type: TypeString, Default: "fw-download-alias1.raspberrypi.com",
		Description: "Server for network install / HTTP boot downloads.",
	},
	{
		Name: "HTTP_PORT", Type: TypeInt, Default: "443",
		Description: "Port for network install (443 with HTTPS, 80 with plain HTTP).",
	},
	{
		Name: "HTTP_PATH", Type: TypeString, Default: "net_install",
		Description: "Path for boot.img download (case-sensitive, forward slashes).",
	},
	{
		Name: "IMAGER_REPO_URL", Type: TypeString, Default: "",
		Description: "JSON file URL for embedded Raspberry Pi Imager during network install.",
	},
	{
		Name: "NET_INSTALL_ENABLED", Type: TypeBool, Default: "1",
		Description: "Display network-install UI on boot when a keyboard is detected.",
	},
	{
		Name: "NET_INSTALL_AT_POWER_ON", Type: TypeBool, Default: "0",
		Description: "Briefly display network-install UI on every cold boot.",
	},
	{
		Name: "NET_INSTALL_KEYBOARD_WAIT", Type: TypeInt, Default: "900",
		Description: "Keyboard-detection wait in ms; 0 disables (minimum ~750ms).",
	},

	// --- Conditional ---
	{
		Name: "BOOTVAR0", Type: TypeInt, Default: "0",
		Description: "Conditional variable for config.txt to switch on.",
	},

	// --- Power supply ---
	{
		Name: "PSU_MAX_CURRENT", Type: TypeInt, Default: "",
		Description:   "Skip USB negotiation and assume current rating (3000 or 5000 mA).",
		OnlyPlatforms: []Platform{PlatformRPi5},
	},

	// --- USB mass-storage boot ---
	{
		Name: "USB_MSD_EXCLUDE_VID_PID", Type: TypeString, Default: "",
		Description: "Up to four VID/PID pairs to ignore during USB-MSD enumeration.",
	},
	{
		Name: "USB_MSD_DISCOVER_TIMEOUT", Type: TypeInt, Default: "20000",
		Description: "USB-MSD detection timeout in ms (min 5000).",
	},
	{
		Name: "USB_MSD_LUN_TIMEOUT", Type: TypeInt, Default: "2000",
		Description: "Timeout before advancing to next LUN in ms (min 100).",
	},
	{
		Name: "USB_MSD_PWR_OFF_TIME", Type: TypeInt, Default: "1000",
		Description:   "USB power-off duration on reboot in ms (0–5000).",
		OnlyPlatforms: []Platform{PlatformRPi4},
	},
	{
		Name: "USB_MSD_STARTUP_DELAY", Type: TypeInt, Default: "0",
		Description: "Delay USB enumeration for slow initialization in ms (0–30000).",
	},

	// --- USB host / debug ---
	{
		Name: "VL805", Type: TypeBool, Default: "0",
		Description: "Load VL805 firmware from EEPROM (CM4 only).",
	},
	{
		Name: "XHCI_DEBUG", Type: TypeHex, Default: "0x0",
		Description: "USB debug verbosity (0x1 descriptors, 0x2 state machine, 0x8 all).",
	},

	// --- SDRAM ---
	{
		Name: "SDRAM_BANKLOW", Type: TypeInt, Default: "",
		Description: "SDRAM bank address mapping (0–4); unset = bootloader recommendation.",
	},
}

// All returns the entire catalog, defensively copied.
func All() []Key {
	out := make([]Key, len(catalog))
	copy(out, catalog)
	return out
}

// ForPlatform returns the subset of the catalog that applies to platform p.
func ForPlatform(p Platform) []Key {
	out := make([]Key, 0, len(catalog))
	for _, k := range catalog {
		if k.AppliesTo(p) {
			out = append(out, k)
		}
	}
	return out
}

// Lookup finds a key by name in the catalog (case-sensitive — matches the
// upstream all-caps convention). Returns ok=false when unknown.
func Lookup(name string) (Key, bool) {
	for _, k := range catalog {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// IsDefault reports whether `value` (after type-aware normalisation) equals
// the documented default for `key` on platform p. False for unknown keys,
// keys that don't apply to p, or keys whose documented default is empty
// (meaning "no default — leave unset by default").
func IsDefault(p Platform, keyName, value string) bool {
	k, ok := Lookup(keyName)
	if !ok || !k.AppliesTo(p) || k.Default == "" {
		return false
	}
	return EqualValues(k.Type, value, k.Default)
}

// EqualValues compares two textual values under the type's normalisation
// rules. Empty values are never equal (a present key with empty value is
// semantically different from absence — we let the caller decide).
func EqualValues(t Type, a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return a == b
	}
	switch t {
	case TypeBool, TypeInt:
		ai, aerr := strconv.ParseInt(a, 0, 64) // accepts 0x… and plain
		bi, berr := strconv.ParseInt(b, 0, 64)
		if aerr == nil && berr == nil {
			return ai == bi
		}
		// fall through to string compare on parse failure
	case TypeHex:
		// Strip optional 0x/0X prefix, compare case-insensitively as int.
		na := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(a), "0x"), "0X")
		nb := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(b), "0x"), "0X")
		ai, aerr := strconv.ParseUint(na, 16, 64)
		bi, berr := strconv.ParseUint(nb, 16, 64)
		if aerr == nil && berr == nil {
			return ai == bi
		}
	}
	return a == b
}

// FilterDefaultsFromBootconf rewrites a bootconf.txt-shaped INI blob by
// removing key=value lines in the [all] section whose value matches the
// documented default for platform p. Other sections ([gpio4=1] etc.) are
// preserved verbatim because their predicate changes the meaning of the
// value — documentation defaults don't apply inside them.
//
// Comments, blank lines, and unknown keys are preserved. Sections are
// emitted in source order. A trailing newline is ensured.
func FilterDefaultsFromBootconf(p Platform, content string) string {
	type entry struct {
		raw   string // original line, preserved verbatim if kept
		isHdr bool   // true for "[section]" lines
		drop  bool   // true if a key=value line matches the default
	}

	var lines []entry
	section := "all"
	scanner := newLineScanner(content)
	for scanner.scan() {
		raw := scanner.text()
		trimmed := strings.TrimSpace(raw)

		// Comments / blanks pass through.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			lines = append(lines, entry{raw: raw})
			continue
		}

		// Section header — track and pass through.
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = normaliseSectionName(trimmed[1 : len(trimmed)-1])
			lines = append(lines, entry{raw: raw, isHdr: true})
			continue
		}

		// key=value — only filter when inside [all].
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			lines = append(lines, entry{raw: raw})
			continue
		}
		if section != "all" {
			lines = append(lines, entry{raw: raw})
			continue
		}
		keyName := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimSpace(trimmed[eq+1:])
		if IsDefault(p, keyName, value) {
			lines = append(lines, entry{raw: raw, drop: true})
			continue
		}
		lines = append(lines, entry{raw: raw})
	}

	var out strings.Builder
	for _, l := range lines {
		if l.drop {
			continue
		}
		out.WriteString(l.raw)
		if !strings.HasSuffix(l.raw, "\n") {
			out.WriteString("\n")
		}
	}
	result := out.String()
	if result == "" {
		return ""
	}
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// SerializeAllSection emits a bootconf.txt-shaped [all] section from the
// given key/value map. Drops entries whose value matches the documented
// default for platform p (same rule as FilterDefaultsFromBootconf).
// Keys are sorted alphabetically for stable output.
//
// existingNonAll is optional: pass the verbatim text of any non-[all]
// sections you want to keep (e.g. when servicing a PATCH that only
// touches the [all] keys). Those lines are appended after the new
// [all] block, preserving conditional sections the user has set up.
func SerializeAllSection(p Platform, attrs map[string]string, existingNonAll string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("[all]\n")
	for _, k := range keys {
		v := strings.TrimSpace(attrs[k])
		if IsDefault(p, k, v) {
			continue
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if existingNonAll != "" {
		if !strings.HasPrefix(existingNonAll, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(existingNonAll)
		if !strings.HasSuffix(existingNonAll, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ExtractNonAllSections returns just the text of sections OTHER than [all],
// preserving original formatting (headers + content) and ordering. Used by
// the Redfish PATCH path so an attribute-map replacement of [all] doesn't
// wipe out the user's conditional sections.
func ExtractNonAllSections(content string) string {
	var b strings.Builder
	section := "all"
	keep := false
	scanner := newLineScanner(content)
	for scanner.scan() {
		raw := scanner.text()
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = normaliseSectionName(trimmed[1 : len(trimmed)-1])
			keep = section != "all"
			if keep {
				b.WriteString(raw)
				if !strings.HasSuffix(raw, "\n") {
					b.WriteByte('\n')
				}
			}
			continue
		}
		if keep {
			b.WriteString(raw)
			if !strings.HasSuffix(raw, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// ParseAllSection returns key→value pairs in the [all] section of content.
// Conditional sections are ignored. Used by GET /Bios to surface a flat
// Attributes map.
func ParseAllSection(content string) map[string]string {
	out := make(map[string]string)
	section := "all"
	scanner := newLineScanner(content)
	for scanner.scan() {
		line := strings.TrimSpace(scanner.text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = normaliseSectionName(line[1 : len(line)-1])
			continue
		}
		if section != "all" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if key != "" {
			out[key] = value
		}
	}
	return out
}

// normaliseSectionName trims + lowercases a section header so [All],
// [ALL], and [ all ] all compare equal to "all". The bootloader treats
// section predicates case-insensitively in practice, and upstream tooling
// (rpi-eeprom-config, hand-edited configs) uses a mix.
func normaliseSectionName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "all"
	}
	return s
}

// ListSections returns the (lowercased, deduped, source-order) list of
// section names a bootconf.txt-shaped blob contains. If the file has any
// key=value lines before the first explicit [section] header, "all" is
// included implicitly. Used by diagnostics to spot "settings all live in
// a conditional section" cases.
func ListSections(content string) []string {
	seen := map[string]bool{}
	order := []string{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		order = append(order, name)
	}

	section := ""
	scanner := newLineScanner(content)
	for scanner.scan() {
		line := strings.TrimSpace(scanner.text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = normaliseSectionName(line[1 : len(line)-1])
			add(section)
			continue
		}
		// key=value before any header → implicit [all].
		if section == "" {
			section = "all"
			add(section)
		}
	}
	return order
}

// --- tiny line scanner (avoids bufio.Scanner's drop-trailing-newline quirk) ---

type lineScanner struct {
	src    string
	cursor int
	cur    string
}

func newLineScanner(s string) *lineScanner { return &lineScanner{src: s} }

func (l *lineScanner) scan() bool {
	if l.cursor >= len(l.src) {
		return false
	}
	end := strings.IndexByte(l.src[l.cursor:], '\n')
	if end < 0 {
		l.cur = l.src[l.cursor:]
		l.cursor = len(l.src)
		return true
	}
	// include the newline in `cur` so writers preserve original line endings
	l.cur = l.src[l.cursor : l.cursor+end+1]
	l.cursor += end + 1
	return true
}

func (l *lineScanner) text() string { return l.cur }
