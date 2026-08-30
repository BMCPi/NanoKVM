package config

import "fmt"

type Config struct {
	Proto          string    `yaml:"proto"`
	Port           Port      `yaml:"port"`
	Cert           Cert      `yaml:"cert"`
	Logger         Logger    `yaml:"logger"`
	Authentication string    `yaml:"authentication"`
	JWT            JWT       `yaml:"jwt"`
	Stun           string    `yaml:"stun"`
	Turn           Turn      `yaml:"turn"`
	Security       Security  `yaml:"security"`
	IPMI           IPMI      `yaml:"ipmi"`
	Redfish        Redfish   `yaml:"redfish"`
	Serial         Serial    `yaml:"serial"`
	Console        Console   `yaml:"console"`
	SSH            SSH       `yaml:"ssh"`
	Firmware       Firmware  `yaml:"firmware"`
	UsbGadget      UsbGadget `yaml:"usbGadget"`

	Power      Power      `yaml:"power"`
	Telemetry  Telemetry  `yaml:"telemetry"`
	AutoUpdate AutoUpdate `yaml:"autoUpdate"`
	// MDNS is the pre-discovery top-level spelling, kept only as the landing
	// spot viper unmarshals a legacy server.yaml's mdns: block into — see
	// migrateDiscovery. Discovery.MDNS is what the mDNS/SSDP responders read.
	MDNS      MDNS      `yaml:"mdns"`
	Discovery Discovery `yaml:"discovery"`
	Network   Network   `yaml:"network"`
	TimeSync  TimeSync  `yaml:"timeSync"`
	Hardware  Hardware  `yaml:"-"`

	// Macros are the operator's keyboard macros (see macros.go). Stored with
	// the config so every client and every session sees the same set.
	Macros []KeyboardMacro `yaml:"macros" json:"macros"`
}

// TimeSync configures the built-in SNTP client (pkg/timesync) that
// replaces busybox ntpd on the image. Sources are tried in a fixed order:
// Servers below, NTP servers from the eth0 DHCP lease, well-known fallback
// IPs/hostnames, and finally HTTP Date headers (for networks blocking
// UDP/123).
type TimeSync struct {
	// Enabled gates the whole subsystem; when false the clock is never touched.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Servers are operator-provided NTP servers (IP or hostname), tried before
	// every other source.
	Servers []string `yaml:"servers" json:"servers"`
	// IntervalMinutes between periodic re-syncs after the first success.
	// Failed attempts retry on a 5s..5m backoff regardless of this value.
	IntervalMinutes int `yaml:"intervalMinutes" json:"intervalMinutes"`
}

// Network configures the host-facing interfaces the BMC brings up directly via
// netlink (pkg/network), replacing the shell ip/udhcpc/ifupdown
// setup. Modeled on jetkvm-community/kvm's split: a full static-or-DHCP wired
// uplink (eth0) and a static link-local USB Redfish-Host-Interface link (usb0).
type Network struct {
	// Enabled gates the whole subsystem. When false the server configures no
	// interfaces (e.g. when addressing is still owned by init scripts).
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Eth0 is the primary wired uplink.
	Eth0 InterfaceConfig `yaml:"eth0" json:"eth0"`
	// RHI is the USB host-facing management link (the ncm gadget's usb0), a
	// point-to-point IPv4 link-local segment in the Redfish Host Interface
	// (DSP0270) style: no gateway, and only a single-lease DHCP server that
	// hands the host a peer address with no router/DNS options — so the link
	// can never affect BMC (or host) routing. A host without a DHCP client
	// still works by self-assigning an APIPA address in the same /16. Matches
	// JetKVM's usb0 handling.
	RHI RHIConfig `yaml:"rhi" json:"rhi"`
}

// InterfaceConfig is the addressing policy for a wired interface.
type InterfaceConfig struct {
	// Name is the kernel interface name (e.g. "eth0").
	Name string `yaml:"name" json:"name"`
	// Mode is "dhcp" (default) or "static".
	Mode string `yaml:"mode" json:"mode"`
	// MAC optionally overrides the link's hardware address so the DHCP
	// lease/identity is stable across reboots. Empty keeps the kernel MAC.
	MAC string `yaml:"mac" json:"mac"`
	// Address is the static CIDR (e.g. "192.168.1.50/24"), used only when
	// Mode == "static".
	Address string `yaml:"address" json:"address"`
	// Gateway is the static default-route next hop. Empty adds no default route.
	Gateway string `yaml:"gateway" json:"gateway"`
	// DNS are the nameservers written to /etc/resolv.conf in static mode.
	DNS []string `yaml:"dns" json:"dns"`
}

// RHIConfig is the static link-local addressing for the USB host interface.
type RHIConfig struct {
	// Interface is the gadget netdev name (the ncm function registers usb0).
	Interface string `yaml:"interface" json:"interface"`
	// Address is the BMC-side CIDR on the link (default 169.254.10.1/16, per
	// RFC 3927 link-local so the host stays reachable even on an IPv4LL host).
	Address string `yaml:"address" json:"address"`
	// Lease is the single fixed address (default 169.254.10.2) the in-process
	// DHCP server offers the host — with subnet/lease options only, never a
	// router or DNS, so the host cannot route through the BMC. Empty disables
	// the server; hosts then rely on IPv4LL self-assignment.
	Lease string `yaml:"lease" json:"lease"`
}

// MDNS configures the built-in multicast-DNS responder that advertises the
// device's <hostname>.local A/AAAA record on the LAN. It replaces avahi-daemon;
// like avahi on this image it only answers hostname queries — no service/TXT
// records. The responder is scoped to a single interface (eth0 by default) so
// the point-to-point USB host link (usb0, 169.254.10.1) never receives
// duplicate records for the managed host. Mirrors the JetKVM internal/mdns
// pion/mdns responder pattern.
type MDNS struct {
	// Enabled gates the responder. When false, nothing is advertised.
	Enabled bool `yaml:"enabled"`
	// Interface restricts multicast answers to this interface. Empty means all
	// non-loopback, up interfaces (not recommended — would include usb0).
	Interface string `yaml:"interface"`
	// IPv4/IPv6 select which multicast responders to bind (224.0.0.251:5353 and
	// [ff02::fb]:5353). Each bind is best-effort; a failure on one leaves the
	// other serving.
	IPv4 bool `yaml:"ipv4"`
	IPv6 bool `yaml:"ipv6"`
	// Hostname overrides the advertised name. Empty = the OS hostname
	// (/etc/hostname); the responder appends ".local".
	Hostname string `yaml:"hostname"`
}

// Discovery groups the responders that make the BMC findable on the LAN
// without an operator already knowing its address: mDNS (hostname
// resolution) and SSDP (device-type/service announcement for Redfish
// discovery tooling). It replaced the top-level mdns: block; see
// migrateDiscovery in default.go for how an older config still works.
type Discovery struct {
	MDNS MDNS `yaml:"mdns"`
	SSDP SSDP `yaml:"ssdp"`
}

// SSDP configures the UPnP/SSDP announce-and-respond service used by Redfish
// discovery tooling (DSP0263) to find the BMC without an operator already
// knowing its address.
type SSDP struct {
	// Enabled gates the whole subsystem. When false, nothing is announced and
	// M-SEARCH requests go unanswered.
	Enabled bool `yaml:"enabled"`
	// Interface restricts announcements/responses to this interface. Empty
	// inherits Discovery.MDNS.Interface, so the two responders are scoped
	// together by default instead of one silently covering usb0.
	Interface string `yaml:"interface"`
	// MaxAge is the advertised cache-control lifetime in seconds. 0 means the
	// default of 1800 (the UPnP-recommended minimum).
	MaxAge int `yaml:"maxAge"`
}

// AutoUpdate configures the background updater that periodically checks for
// new application releases and applies them when enabled. Disabled by default
// — opt-in via config or the settings dialog. Host firmware is deliberately
// out of scope: it is delivered as operator-staged FMP capsules (see Firmware),
// never fetched and applied behind the operator's back.
type AutoUpdate struct {
	// Enabled gates the whole subsystem; when false the ticker doesn't run.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IntervalMinutes between check-and-apply runs. Clamped to >= 5 at runtime
	// so a misconfigured value can't hammer GitHub.
	IntervalMinutes int `yaml:"intervalMinutes" json:"intervalMinutes"`
	// Application toggles auto-updating the NanoKVM application package.
	Application bool `yaml:"application" json:"application"`
}

// Telemetry holds OpenTelemetry + Prometheus configuration.
//
// When Enabled is true:
//   - Gin HTTP handlers are auto-instrumented (request count, latency, traces).
//   - If Prometheus.Enabled, the OTel Prometheus exporter is served at
//     Prometheus.Path on the existing HTTP server (default /metrics).
//   - If OTLP.Endpoint is non-empty, traces and metrics are exported via OTLP
//     gRPC to that endpoint (e.g. otel-collector:4317).
type Telemetry struct {
	Enabled     bool       `yaml:"enabled"`
	ServiceName string     `yaml:"serviceName"`
	Prometheus  Prometheus `yaml:"prometheus"`
	OTLP        OTLP       `yaml:"otlp"`
}

type Prometheus struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// OTLP configures the OpenTelemetry Protocol exporter (gRPC).
// Insecure=true sends plaintext (suitable for sidecar collectors on localhost).
type OTLP struct {
	Endpoint string `yaml:"endpoint"`
	Insecure bool   `yaml:"insecure"`
}

type Logger struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type Port struct {
	Http  int `yaml:"http"`
	Https int `yaml:"https"`
}

type Cert struct {
	Crt string `yaml:"crt"`
	Key string `yaml:"key"`
}

type JWT struct {
	SecretKey            string `yaml:"secretKey"`
	RefreshTokenDuration uint64 `yaml:"refreshTokenDuration"`
	RevokeTokensOnLogout bool   `yaml:"revokeTokensOnLogout"`
}

type Turn struct {
	TurnAddr string `yaml:"turnAddr"`
	TurnUser string `yaml:"turnUser"`
	TurnCred string `yaml:"turnCred"`
}

type Security struct {
	LoginLockoutDuration int `yaml:"loginLockoutDuration"`
	LoginMaxFailures     int `yaml:"loginMaxFailures"`
}

// GPIOPin identifies a GPIO line via the character-device (CONFIG_GPIO_CDEV)
// interface: a gpiochip plus the line's offset within that chip. This replaces
// the deprecated sysfs numbering (/sys/class/gpio/gpioN/value, CONFIG_GPIO_SYSFS).
//
// Chip may be a bare name ("gpiochip0") or a device path ("/dev/gpiochip0").
type GPIOPin struct {
	Chip string
	Line int
}

// IsZero reports whether the pin is unset (no chip configured).
func (p GPIOPin) IsZero() bool { return p.Chip == "" }

// String renders the pin as chip:line for logs and errors.
func (p GPIOPin) String() string {
	if p.IsZero() {
		return "<unset>"
	}
	return fmt.Sprintf("%s:%d", p.Chip, p.Line)
}

type Hardware struct {
	Version      HWVersion `yaml:"-"`
	GPIOReset    GPIOPin   `yaml:"-"`
	GPIOPower    GPIOPin   `yaml:"-"`
	GPIOPowerLED GPIOPin   `yaml:"-"`
	GPIOHDDLed   GPIOPin   `yaml:"-"`
}

// Power holds power-control configuration.
// LegacyMode opts into direct-GPIO control (cuts power pin directly) instead of
// the default button-press simulation via the power-LED header.
type Power struct {
	LegacyMode bool `yaml:"legacyMode"`
}

type IPMI struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
	// Username/Password authenticate RMCP+ (lanplus) sessions. They are a
	// separate credential from the web/Redfish accounts, which are stored
	// bcrypt-hashed: RAKP authentication HMACs the password itself, so the
	// IPMI secret has to be recoverable.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Redfish struct {
	Enabled bool `yaml:"enabled"`
}

type Serial struct {
	Device      string `yaml:"device"`
	BaudRate    int    `yaml:"baudRate"`
	Parity      string `yaml:"parity"`
	DataBits    int    `yaml:"dataBits"`
	StopBits    int    `yaml:"stopBits"`
	FlowControl string `yaml:"flowControl"`
	// Capture continuously records the host's serial output to a bounded
	// file, so its boot logs are retained even when no console session is
	// watching. While enabled the port is held open for the whole server
	// lifetime (the capture registers as a permanent broker session).
	Capture SerialCapture `yaml:"capture"`
}

// SerialCapture bounds the always-on host-console capture.
type SerialCapture struct {
	// Enabled gates the capture (default true).
	Enabled bool `yaml:"enabled"`
	// File is the capture path — on the persistent data partition so the
	// managed host's boot/crash logs survive a BMC reboot.
	File string `yaml:"file"`
	// MaxSizeKB caps the file; on overflow it rotates once to File+".1",
	// so at most twice this is ever retained.
	MaxSizeKB int `yaml:"maxSizeKB"`
}

// Console configures how the dashboard presents the managed host.
//
// The BMC can show the host two ways -- its serial console over the UART, and
// its HDMI output captured through the video pipeline -- and which one is
// "the console" is a property of the machine being managed, not of the person
// looking at it. A headless server is a serial box whose HDMI port is dark; a
// workstation is the reverse. So this is persisted device configuration rather
// than a per-browser preference.
type Console struct {
	// PrimaryView selects the view the dashboard opens on: PrimaryViewSerial
	// or PrimaryViewHDMI. Both views are always reachable as tabs; this only
	// decides which one is in front on load.
	//
	// It defaults to serial because serial is the view that always works:
	// the UART needs no capture pipeline, no HDMI cable and no signal from
	// the host, so a fresh device shows something useful before anyone has
	// confirmed video is wired up at all.
	PrimaryView string `yaml:"primaryView"`
}

// Valid values for Console.PrimaryView.
const (
	PrimaryViewSerial = "serial"
	PrimaryViewHDMI   = "hdmi"
)

// HDMIPrimary reports whether the dashboard should open on the HDMI view.
//
// Anything that is not the HDMI sentinel reads as serial, so an unset field or
// a hand-edited typo in server.yaml degrades to the view that always works
// rather than to a black pane.
func (c Console) HDMIPrimary() bool { return c.PrimaryView == PrimaryViewHDMI }

// SSH configures the in-process SSH server (pkg/ssh). The BMC
// ships no sshd — the server implements the transport itself with
// golang.org/x/crypto/ssh and runs sessions on the shared PTY plumbing
// (pkg/shell), the same code the web terminal drawer uses.
//
// Enabled is what the Settings dialog's SSH switch writes, so it is persisted
// to /etc/kvm/server.yaml (a bind mount onto the data partition) rather than
// tracked by a flag file.
type SSH struct {
	// Enabled gates the listener. Toggling it starts/stops the server without
	// a restart.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Port to listen on. 22 by default.
	Port int `yaml:"port" json:"port"`
	// HostKeyPath is the server's private host key. Generated (ed25519) on
	// first start if missing. It must live on the persistent data partition:
	// the root overlay is volatile, so a key under /etc would be regenerated
	// every boot and every reconnecting client would see a host-key change.
	HostKeyPath string `yaml:"hostKeyPath" json:"-"`
	// AuthorizedKeysPath holds the client public keys allowed to log in, in
	// OpenSSH authorized_keys format. Managed through /api/vm/ssh/keys.
	AuthorizedKeysPath string `yaml:"authorizedKeysPath" json:"-"`
	// PasswordAuth additionally accepts the BMC web credentials (the same
	// account Redfish/IPMI/Basic-Auth use) as an SSH password. Public-key auth
	// is always available; set this false to require keys.
	PasswordAuth bool `yaml:"passwordAuth" json:"passwordAuth"`
}

// Firmware configures how the BMC delivers firmware to the managed host.
//
// The BMC no longer serves a bootable host image over the USB gadget: that
// path is retired. The host boots its own firmware, and updates are delivered
// as UEFI FMP capsules using the specification's standard "Delivering Capsules
// Across a System Reset" mechanism (UEFI 2.10 §8.5.5).
//
// The BMC keeps a small GPT disk image (CapsulePath) holding one EFI System
// Partition formatted FAT32. Capsules are staged into \EFI\UpdateCapsule\ on
// it and the image is presented on the mass-storage gadget's lun.0. At the
// host's next boot its firmware scans the attached FAT volumes, finds the
// capsules and applies them via FMP; nothing on the BMC flashes the host.
type Firmware struct {
	// CapsulePath is the GPT capsule volume image presented on lun.0. It is
	// created on first run if absent.
	CapsulePath string `yaml:"capsulePath"`
	// CapsuleSizeMB is the size of the capsule volume when it is first
	// created. Ignored once the image exists — delete the file to resize.
	// Floored at capsuleMinSizeMB so the ESP is a spec-legal FAT32 volume.
	CapsuleSizeMB int `yaml:"capsuleSizeMB"`
	// MediaDir is the directory where ISO images for virtual media are stored.
	MediaDir string `yaml:"mediaDir"`
}

// UsbGadget configures the USB device gadget (g0) that the BMC presents to the
// managed host, and is the sole source of truth for which functions it exposes.
// The Go server owns the gadget's configfs tree (/sys/kernel/config/usb_gadget/
// g0): it builds the gadget from this config at startup and reconciles it when
// these fields change. This replaced the packaging/etc/init.d/S03usbdev shell
// script, the ad-hoc /boot flag files, and the separate runtime state file that
// used to drive it; see the pkg/usbgadget package.
type UsbGadget struct {
	// Enabled gates the whole subsystem. When false the server does not touch
	// the gadget configfs at all (e.g. boards without a device-mode UDC).
	Enabled bool `yaml:"enabled"`

	// USB device-descriptor identity. VendorID/ProductID are hex strings
	// ("0x3346"/"0x1009") written verbatim to idVendor/idProduct.
	VendorID     string `yaml:"vendorID"`
	ProductID    string `yaml:"productID"`
	SerialNumber string `yaml:"serialNumber"`
	Manufacturer string `yaml:"manufacturer"`
	Product      string `yaml:"product"`

	// Configuration descriptor attributes for configs/c.1. BmAttributes is a
	// hex string ("0xE0" = bus-powered + remote wakeup); MaxPower is in mA/2
	// units as configfs expects (120).
	MaxPower     int    `yaml:"maxPower"`
	BmAttributes string `yaml:"bmAttributes"`

	// Ethernet selects the USB network function exposed to the host: "off",
	// "ncm" (CDC-NCM). Toggled at runtime via the virtual-
	// device API, which persists the change back here. Formerly the
	// the runtime state file.
	Ethernet string `yaml:"ethernet"`
	// Disk controls whether the mass-storage disk (mass_storage.disk0) is linked
	// into configs/c.1 and so visible to the host. The function and its LUNs
	// always exist — the FMP capsule volume lives on lun.0 — this only gates the
	// symlink. Toggled at runtime like Ethernet. Formerly the state file's disk
	// toggle.
	Disk bool `yaml:"disk"`

	// HID enables the combined keyboard/mouse/touchpad function (hid.GS0). The
	// HID report stream is multiplexed over /dev/hidg0 with Report IDs; the
	// gadget creates the function with the combined report descriptor.
	HID bool `yaml:"hid"`
	// BIOSMode sets subclass=1 on the HID functions (boot-protocol compatible
	// for BIOS/UEFI setup screens). Formerly the /boot/BIOS flag.
	BIOSMode bool `yaml:"biosMode"`
	// WakeupOnWrite sets wakeup_on_write=1 on the HID functions so host writes
	// can wake a suspended host. Formerly the absence of /boot/usb.notwakeup.
	WakeupOnWrite bool `yaml:"wakeupOnWrite"`

	// BindUDC binds the gadget to a UDC at startup. Formerly the absence of
	// /boot/udc.disable.
	BindUDC bool `yaml:"bindUDC"`
	// UDCName selects the UDC to bind. Empty = auto-detect the first entry in
	// /sys/class/udc (this board's dwc2 controller is "4340000.usb").
	UDCName string `yaml:"udcName"`
	// OTGRolePath is the CVITEK/Sophgo OTG role switch. The gadget writes
	// "device" here after binding so the controller acts as a peripheral.
	OTGRolePath string `yaml:"otgRolePath"`
	// PHYDevice is the platform device name rebound on RebindPHY() recovery
	// (dwc2 driver unbind/bind).
	PHYDevice string `yaml:"phyDevice"`
}
