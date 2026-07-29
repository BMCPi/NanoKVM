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
	Firmware       Firmware  `yaml:"firmware"`
	UsbGadget      UsbGadget `yaml:"usbGadget"`
	EfiVars        EfiVars   `yaml:"efiVars"`
	UbootEnv       UbootEnv  `yaml:"ubootEnv"`
	SMBIOS         SMBIOS    `yaml:"smbios"`

	Power      Power      `yaml:"power"`
	Telemetry  Telemetry  `yaml:"telemetry"`
	AutoUpdate AutoUpdate `yaml:"autoUpdate"`
	MDNS       MDNS       `yaml:"mdns"`
	Network    Network    `yaml:"network"`
	Hardware   Hardware   `yaml:"-"`
}

// Network configures the host-facing interfaces the BMC brings up directly via
// netlink (server/service/network), replacing the shell ip/udhcpc/ifupdown
// setup. Modeled on jetkvm-community/kvm's split: a full static-or-DHCP wired
// uplink (eth0) and a static link-local USB Redfish-Host-Interface link (usb0).
type Network struct {
	// Enabled gates the whole subsystem. When false the server configures no
	// interfaces (e.g. when addressing is still owned by init scripts).
	Enabled bool `yaml:"enabled"`
	// Eth0 is the primary wired uplink.
	Eth0 InterfaceConfig `yaml:"eth0"`
	// RHI is the USB host-facing management link (the ecm/ncm gadget's usb0), a
	// point-to-point IPv4 link-local segment in the Redfish Host Interface
	// (DSP0270) style: no gateway and no DHCP server, so it can never affect BMC
	// routing and the attached host self-assigns an APIPA address in the same
	// /16. Matches JetKVM's usb0 handling.
	RHI RHIConfig `yaml:"rhi"`
}

// InterfaceConfig is the addressing policy for a wired interface.
type InterfaceConfig struct {
	// Name is the kernel interface name (e.g. "eth0").
	Name string `yaml:"name"`
	// Mode is "dhcp" (default) or "static".
	Mode string `yaml:"mode"`
	// MAC optionally overrides the link's hardware address so the DHCP
	// lease/identity is stable across reboots. Empty keeps the kernel MAC.
	MAC string `yaml:"mac"`
	// Address is the static CIDR (e.g. "192.168.1.50/24"), used only when
	// Mode == "static".
	Address string `yaml:"address"`
	// Gateway is the static default-route next hop. Empty adds no default route.
	Gateway string `yaml:"gateway"`
	// DNS are the nameservers written to /etc/resolv.conf in static mode.
	DNS []string `yaml:"dns"`
}

// RHIConfig is the static link-local addressing for the USB host interface.
type RHIConfig struct {
	// Interface is the gadget netdev name (the ecm/ncm function registers usb0).
	Interface string `yaml:"interface"`
	// Address is the BMC-side CIDR on the link (default 169.254.10.1/16, per
	// RFC 3927 link-local so the host stays reachable even on an IPv4LL host).
	Address string `yaml:"address"`
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

// AutoUpdate configures the background updater that periodically checks
// for new application and BIOS (U-Boot) releases and applies them when
// enabled. Disabled by default — opt-in via config or the settings dialog.
type AutoUpdate struct {
	// Enabled gates the whole subsystem; when false the ticker doesn't run.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IntervalMinutes between check-and-apply runs. Clamped to >= 5 at runtime
	// so a misconfigured value can't hammer GitHub.
	IntervalMinutes int `yaml:"intervalMinutes" json:"intervalMinutes"`
	// Application toggles auto-updating the NanoKVM application package.
	Application bool `yaml:"application" json:"application"`
	// BIOS toggles auto-updating the U-Boot BIOS image.
	BIOS bool `yaml:"bios" json:"bios"`
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
}

type Firmware struct {
	ImageURL  string `yaml:"imageURL"`
	ImagePath string `yaml:"imagePath"`
	// FirmwareDir is the local directory holding the canonical FAT root files
	// (u-boot.bin, config.txt, RPi *.elf/*.dat firmware blobs, .dtb files,
	// overlays/, etc.). The boot image is built from this directory; it is
	// the source of truth, allowing each file to be versioned/edited
	// independently of the composite .img.
	FirmwareDir string `yaml:"firmwareDir"`
	// MountPoint is retained for backward-compat with existing YAML files but
	// is no longer used at runtime — env paths are derived as FAT-root names.
	MountPoint string `yaml:"mountPoint"`
	// MachineEnv, PersistentEnv and OnceEnv are retained for backward-compat
	// with existing YAML files but are no longer used at runtime: the U-Boot
	// environment lives in the I2C EEPROM (see UbootEnv), not in files inside
	// the boot image.
	MachineEnv    string `yaml:"machineEnv"`
	PersistentEnv string `yaml:"persistentEnv"`
	OnceEnv       string `yaml:"onceEnv"`
	// MediaDir is the directory where ISO images for virtual media are stored.
	MediaDir string `yaml:"mediaDir"`
}

// UsbGadget configures the USB device gadget (g0) that the BMC presents to the
// managed host, and is the sole source of truth for which functions it exposes.
// The Go server owns the gadget's configfs tree (/sys/kernel/config/usb_gadget/
// g0): it builds the gadget from this config at startup and reconciles it when
// these fields change. This replaced the packaging/etc/init.d/S03usbdev shell
// script, the ad-hoc /boot flag files, and the separate runtime state file that
// used to drive it; see the server/service/usbgadget package.
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
	// "ecm" (CDC-ECM) or "ncm" (CDC-NCM). Toggled at runtime via the virtual-
	// device API, which persists the change back here. Formerly the
	// /boot/usb.ecm0 and /boot/usb.ncm flags plus the runtime state file.
	Ethernet string `yaml:"ethernet"`
	// Disk controls whether the mass-storage disk (mass_storage.disk0) is linked
	// into configs/c.1 and so visible to the host. The function and its LUNs
	// always exist — the firmware boot image lives on lun.0 — this only gates the
	// symlink. Toggled at runtime like Ethernet. Formerly the state file's disk
	// toggle.
	Disk bool `yaml:"disk"`

	// HID enables the keyboard/mouse/touchpad functions (hid.GS0/1/2). The HID
	// report streams are consumed by a separate component; the gadget only has
	// to create the functions with the correct report descriptors.
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

// EfiVars configures access to the UEFI variable store that U-Boot on the
// host persists in an I2C EEPROM (CONFIG_EFI_VARIABLE_I2C_STORE). The BMC
// reads and rewrites BootOrder/BootNext there out-of-band.
type EfiVars struct {
	// Enabled gates the subsystem; when false Redfish boot overrides fall
	// back to the U-Boot env files.
	Enabled bool `yaml:"enabled"`
	// Path is a file-backed store: the backing file of a kernel
	// i2c-slave-eeprom device (BMC emulating the EEPROM, e.g.
	// /sys/bus/i2c/devices/0-1050/slave-eeprom), an at24 sysfs eeprom node,
	// or a plain file for testing. Takes precedence over I2CBus.
	Path string `yaml:"path"`
	// I2CBus selects raw /dev/i2c-N master access when Path is empty.
	// Set to -1 to disable.
	I2CBus int `yaml:"i2cBus"`
	// I2CAddr is the EEPROM chip address (default 0x50).
	I2CAddr int `yaml:"i2cAddr"`
	// PageSize is the EEPROM write page size in bytes (default 64, 24c256).
	PageSize int `yaml:"pageSize"`
	// StoreSize caps the variable blob size in bytes (default 32768, 24c256).
	StoreSize int `yaml:"storeSize"`
	// SnapshotPath is a durable file on persistent storage that mirrors the
	// store. The kernel i2c-slave-eeprom backing the EEPROM is volatile RAM,
	// wiped on every BMC reboot; the app restores this snapshot into it at
	// startup and re-saves it whenever the host (or the BMC) changes the
	// store, so BootOrder/BootNext survive BMC reboots. Empty disables
	// persistence.
	SnapshotPath string `yaml:"snapshotPath"`
}

// UbootEnv configures where the U-Boot environment lives. U-Boot
// (CONFIG_ENV_IS_IN_EEPROM) keeps it at a fixed offset of the *same* EEPROM
// that holds the UEFI variable store, so this mirrors EfiVars' access fields
// and adds the region within the device:
//
//	0x0000..0x3fff  UEFI variable blob (EfiVars)
//	0x4000..0x7fff  U-Boot environment (this store)
type UbootEnv struct {
	// Enabled gates the subsystem; when false the environment API reports the
	// store as unavailable.
	Enabled bool `yaml:"enabled"`
	// Path is a file-backed store: the backing file of a kernel
	// i2c-slave-eeprom device (BMC emulating the EEPROM), an at24 sysfs
	// eeprom node, or a plain file for testing. Takes precedence over I2CBus.
	Path string `yaml:"path"`
	// I2CBus selects raw /dev/i2c-N master access when Path is empty.
	// Set to -1 to disable.
	I2CBus int `yaml:"i2cBus"`
	// I2CAddr is the EEPROM chip address (default 0x50).
	I2CAddr int `yaml:"i2cAddr"`
	// PageSize is the EEPROM write page size in bytes (default 64, 24c256).
	PageSize int `yaml:"pageSize"`
	// Offset is where the env partition starts in the EEPROM. Must match the
	// host's CONFIG_ENV_OFFSET (default 0x4000).
	Offset int `yaml:"offset"`
	// Size is the env partition size, including its CRC32 header. Must match
	// the host's CONFIG_ENV_SIZE exactly (default 0x2000) — it is the CRC
	// length, not just a bound, so a mismatch makes U-Boot reject an otherwise
	// intact environment with "bad CRC, using default environment". Clamped at
	// SMBIOS.Offset so the region cannot overrun the tables above it.
	Size int `yaml:"size"`
	// SnapshotPath is a durable file mirroring the env region. The kernel
	// i2c-slave-eeprom backing the EEPROM is volatile RAM wiped on every BMC
	// reboot; the app restores this snapshot at startup and re-saves it
	// whenever the host (saveenv) or the BMC changes the environment, so it
	// survives BMC reboots. Empty disables persistence.
	SnapshotPath string `yaml:"snapshotPath"`
}

// SMBIOS configures access to the SMBIOS tables the host's U-Boot mirrors into
// a third region of the same EEPROM (CONFIG_SMBIOS_I2C_STORE):
//
//	0x0000..0x3fff  UEFI variable blob (EfiVars)
//	0x4000..0x5fff  U-Boot environment (UbootEnv)
//	0x6000..0x67ff  SMBIOS tables      (this store)
//
// The tables are the same bytes the host OS sees and carry more than the
// environment does - the type 1 UUID, product name/version, SKU and the
// processor detail exist nowhere else - so inventory prefers them. Read-only:
// only the host writes this region, so there is no snapshot to reconcile.
type SMBIOS struct {
	// Enabled gates the subsystem; when false inventory falls back to the
	// U-Boot environment.
	Enabled bool `yaml:"enabled"`
	// Path is a file-backed store: the backing file of a kernel
	// i2c-slave-eeprom device (BMC emulating the EEPROM), an at24 sysfs
	// eeprom node, or a plain file for testing. Takes precedence over I2CBus.
	Path string `yaml:"path"`
	// I2CBus selects raw /dev/i2c-N master access when Path is empty.
	// Set to -1 to disable.
	I2CBus int `yaml:"i2cBus"`
	// I2CAddr is the EEPROM chip address (default 0x50).
	I2CAddr int `yaml:"i2cAddr"`
	// PageSize is the EEPROM write page size in bytes (default 64, 24c256).
	PageSize int `yaml:"pageSize"`
	// Offset is where the SMBIOS region starts in the EEPROM. Must match the
	// host's CONFIG_SMBIOS_I2C_STORE_OFFSET (default 0x6000).
	Offset int `yaml:"offset"`
	// Size is the SMBIOS region size. Must match the host's
	// CONFIG_SMBIOS_I2C_STORE_SIZE (default 0x800).
	Size int `yaml:"size"`
}
