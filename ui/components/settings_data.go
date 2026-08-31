package components

// settings_data.go holds the view models the settings panels render. They are
// plain data: ui/fragments_settings.go gathers them from pkg/* and passes them
// in, both for the first paint and for every htmx swap, so one template
// definition covers both.

import "github.com/pi-bmc/nanokvm-app/pkg/app/firmware"

// SettingsGeneral backs the General panel — the BMC's own identity plus the
// background updater's configuration.
type SettingsGeneral struct {
	AppVersion   string
	ImageVersion string
	Hardware     string

	AutoUpdateEnabled     bool
	AutoUpdateInterval    string
	AutoUpdateApplication bool

	// ConsolePrimaryView is "serial" or "hdmi" — which view the dashboard
	// opens on. Carried as the raw sentinel so it can be handed straight to
	// the select's Value.
	ConsolePrimaryView string

	// Clock backs the time-synchronisation fieldset. Servers is the operator's
	// list flattened to the comma-separated form the text input round-trips.
	TimeSyncEnabled  bool
	TimeSyncServers  string
	TimeSyncInterval string
}

// SettingsSerial backs the Serial panel — the UART the host console runs on,
// and the always-on capture of its output.
//
// Framing values are carried as strings because they are select values; the
// handler parses them back. CaptureFile is read-only: it is an image layout
// decision (it must sit on the persistent partition or the host's boot logs
// vanish on reboot), not an operator preference.
type SettingsSerial struct {
	Device      string
	BaudRate    string
	DataBits    string
	Parity      string
	StopBits    string
	FlowControl string

	CaptureEnabled bool
	CaptureFile    string
	CaptureMaxKB   string
}

// SettingsNetwork backs the Network panel: the eth0 + USB-host-interface
// configuration form, with the observed state alongside it.
type SettingsNetwork struct {
	Enabled bool
	Mode    string
	MAC     string
	Address string
	Gateway string
	DNS     string

	RHIAddress string
	RHILease   string

	// mDNS is the discovery responder. It is its own form (applied on change)
	// rather than part of the batched addressing form above, because it can be
	// restarted on its own without re-applying addressing and interrupting the
	// session.
	MDNSEnabled   bool
	MDNSHostname  string
	MDNSInterface string
	MDNSIPv4      bool
	MDNSIPv6      bool

	Status SettingsNetworkStatus
}

// IsStatic reports whether the static addressing fields should start visible.
func (n SettingsNetwork) IsStatic() bool { return n.Mode == "static" }

// SettingsNetworkStatus is the read-only counterpart of the network form —
// what the interfaces actually came up with.
type SettingsNetworkStatus struct {
	IP        string
	Interface string
	Mode      string
	MAC       string
	MDNS      string
	RHI       string
}

// SettingsHardware backs the Hardware panel — the USB gadget functions the
// BMC presents to the managed host, plus how power is driven.
//
// The two halves apply differently, which the panel makes visible: Ethernet and
// Disk are reconciled live by the gadget package, while every other field here
// is only read when the gadget tree is built at startup.
type SettingsHardware struct {
	USBNetwork bool
	USBDisk    bool
	MediaState string

	GadgetEnabled bool
	HID           bool
	BIOSMode      bool
	WakeupOnWrite bool

	VendorID     string
	ProductID    string
	Manufacturer string
	Product      string
	SerialNumber string

	PowerLegacyMode bool
}

// SettingsAccess backs the Access panel — every way in, and the policy that
// governs it: the web listener, SSH, login lockout, session lifetime and the
// out-of-band management protocols.
type SettingsAccess struct {
	SSHEnabled      bool
	SSHKeys         string
	SSHPort         string
	SSHPasswordAuth bool

	TLSEnabled bool
	HTTPPort   string
	HTTPSPort  string

	LoginMaxFailures     string
	LoginLockoutDuration string

	SessionDuration    string
	RevokeTokensLogout bool

	IPMIEnabled bool
	IPMIPort    string

	RedfishEnabled bool
}

// SettingsTelemetry backs the collection form at the top of the Metrics panel —
// the switch that decides whether the charts below it have anything to show.
type SettingsTelemetry struct {
	Enabled     bool
	ServiceName string

	PrometheusEnabled bool
	PrometheusPath    string

	OTLPEndpoint string
	OTLPInsecure bool
}

// SettingsFirmware backs the Firmware panel: the capsule volume's state and
// what is queued in it for the managed host to pick up.
//
// The host, not the BMC, applies these — at ITS next boot — and deletes each
// capsule once applied. Staging is therefore never "did it work", only "is it
// queued"; the panel's copy says so rather than implying otherwise.
type SettingsFirmware struct {
	// VolumeReady reports whether the capsule volume has been created on
	// disk yet. False on a freshly flashed card until the first capsule is
	// staged.
	VolumeReady bool
	// Presented reports whether the volume is attached to the gadget's
	// lun.0. A capsule staged onto an unpresented volume is never seen by
	// the host — distinct from VolumeReady, which only says the file exists.
	Presented bool
	// VolumeSize is the capsule volume's size in bytes; meaningful only when
	// VolumeReady.
	VolumeSize int64
	// CapsuleDir is the directory inside the volume the host firmware scans,
	// spelled the way the UEFI spec does (e.g. `\EFI\UpdateCapsule`).
	CapsuleDir string

	// Capsules are what is currently queued for the host's next boot.
	Capsules []firmware.Capsule

	// Staging is true while a URL fetch is in flight (ui/fragments' own
	// tracker, not the controller's — see firmware.go for why the two must
	// not be conflated). StagingName names the capsule being fetched.
	Staging     bool
	StagingName string
}

// SettingsAdvanced backs the Advanced panel.
type SettingsAdvanced struct {
	DeviceKey string

	LogLevel string
	LogFile  string

	Stun     string
	TurnAddr string
	TurnUser string
	TurnCred string

	MediaDir string
}
