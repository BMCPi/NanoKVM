package config

import (
	"fmt"
	"log"
	"strings"

	"github.com/spf13/viper"
)

// defaultInterface is the board's LAN-side kernel interface. Both the mDNS
// responder's scope and the managed eth0 block default to it.
const defaultInterface = "eth0"

var defaultConfig = &Config{
	// HTTPS by default, the way every other BMC ships. Redfish inventory
	// tooling assumes it: OpenCHAMI's magellan scans https://host:443 and,
	// in its collect phase, strips only an "https://" prefix before deriving
	// the endpoint's ID — an http:// endpoint yields an empty ID and is
	// dropped without a diagnostic. A plaintext BMC is invisible to it.
	//
	// This is safe to default on: cmd/server provisions a self-signed
	// certificate when none exists and falls back to plaintext if it cannot,
	// and the :80 listener stays up as a redirect for browsers.
	Proto: "https",
	Port: Port{
		HTTP:  80,
		HTTPS: 443,
	},
	Cert: Cert{
		// Under /etc/kvm, which is bind-mounted from the persistent data
		// partition — the root overlay is volatile, so a relative/rootfs path
		// would silently lose an uploaded certificate on reboot.
		Crt: "/etc/kvm/server.crt",
		Key: "/etc/kvm/server.key",
	},
	Logger: Logger{
		Level: "info",
		// Log to a rotating file (see pkg/logger). /var/log is tmpfs on
		// the device, so logs are RAM-backed and reset each boot; point this
		// at /var/lib/nanokvm to keep them across reboots instead.
		// Set File to "console" to log to stdout instead.
		File: "/var/log/NanoKVM-Server.log",
	},
	JWT: JWT{
		SecretKey:            "",
		RefreshTokenDuration: 2678400,
		RevokeTokensOnLogout: true,
	},
	// No STUN server by default. This field was inherited from upstream
	// NanoKVM, whose console is reachable over the internet through their
	// relay; a BMC and its operator share a management network, where WebRTC
	// connects on host candidates alone. Defaulting to a public server would
	// have every console session announce this device to a third party for no
	// benefit on the network it is deployed on. Set it (and Turn below) when
	// the BMC really is behind NAT from its operators.
	Stun: "",
	Turn: Turn{
		TurnAddr: "",
		TurnUser: "",
		TurnCred: "",
	},
	Authentication: "enable",
	Security: Security{
		LoginLockoutDuration: 0,
		LoginMaxFailures:     5,
	},
	IPMI: IPMI{
		Enabled:  true,
		Port:     623,
		Username: "admin",
		Password: "admin",
	},
	Redfish: Redfish{
		Enabled: true,
	},
	Serial: Serial{
		Device:      "/dev/ttyS1",
		BaudRate:    115200,
		Parity:      "none",
		DataBits:    8,
		StopBits:    1,
		FlowControl: "none",
		Capture: SerialCapture{
			Enabled:   true,
			File:      "/var/lib/nanokvm/log/serial-console.log",
			MaxSizeKB: 1024,
		},
	},
	Console: Console{
		PrimaryView: PrimaryViewSerial,
	},
	// The BMC ships no sshd; the app is the SSH server. Host key and
	// authorized_keys live on the persistent data partition — the root
	// overlay is volatile, so a host key under /etc would change every boot.
	SSH: SSH{
		Enabled:            true,
		Port:               22,
		HostKeyPath:        "/var/lib/nanokvm/ssh/ssh_host_ed25519_key",
		AuthorizedKeysPath: "/var/lib/nanokvm/ssh/authorized_keys",
		PasswordAuth:       true,
	},
	// All firmware/media state lives on the persistent data partition,
	// mounted at /var/lib/nanokvm by the initramfs (the old /data mount is
	// gone with the squashfs+overlay root).
	Firmware: Firmware{
		CapsulePath:   "/var/lib/nanokvm/firmware/capsules.img",
		CapsuleSizeMB: 64,
		MediaDir:      "/var/lib/nanokvm/media",
	},
	UsbGadget: UsbGadget{
		Enabled:       true,
		VendorID:      "0x3346",
		ProductID:     "0x1009",
		SerialNumber:  "0123456789ABCDEF",
		Manufacturer:  "sipeed",
		Product:       "NanoKVM",
		MaxPower:      120,
		BmAttributes:  "0xE0",
		Ethernet:      "ncm", // "off"|"ncm"; matches usbgadget.EthernetNCM
		Disk:          true,
		HID:           true,
		BIOSMode:      true, // boot-subclass HID: EDK2's UsbKbDxe only binds subclass-1 keyboards
		WakeupOnWrite: true,
		BindUDC:       true,
		UDCName:       "", // auto-detect (this board: 4340000.usb)
		OTGRolePath:   "/proc/cviusb/otg_role",
		PHYDevice:     "4340000.usb",
	},
	// No MDNS: here. The legacy top-level block is migration input only, so
	// defaulting it would write the very key migrateDiscovery exists to
	// remove — and would make a default-config boot look like an upgrade.
	Discovery: Discovery{
		MDNS: MDNS{Enabled: true, Interface: defaultInterface, IPv4: true, IPv6: true},
		SSDP: SSDP{Enabled: true, MaxAge: 1800},
	},
	TimeSync: TimeSync{
		Enabled:         true,
		IntervalMinutes: 60,
	},
	Network: Network{
		Enabled: true,
		Eth0: InterfaceConfig{
			Name: defaultInterface,
			Mode: "dhcp",
		},
		RHI: RHIConfig{
			Interface: "usb0",
			Address:   "169.254.10.1/16",
			Lease:     "169.254.10.2",
		},
	},
	Power: Power{
		LegacyMode: false,
		Reset:      PowerResetAuto,
	},
	Telemetry: Telemetry{
		Enabled:     false,
		ServiceName: "nanokvm",
		Prometheus: Prometheus{
			Enabled: true,
			Path:    "/metrics",
		},
		OTLP: OTLP{
			Endpoint: "",
			Insecure: true,
		},
	},
	AutoUpdate: AutoUpdate{
		Enabled:         false,
		IntervalMinutes: 360, // 6 hours
		Application:     true,
	},
}

// checkDefaultValue backfills every field the config file left unset, then
// persists the file once if anything was generated or migrated.
//
// The per-section helpers below are a mechanical split of what used to be one
// function; their call order is load-bearing and must not be rearranged. Two
// dependencies in particular: migrateLegacyDataPaths rewrites the firmware and
// SSH paths that applyFirmwareDefaults/applySSHDefaults have just backfilled,
// and applyDiscoveryDefaults derives the SSDP interface from the mDNS one it
// resolves earlier in the same helper.
//
// applyPowerDefaults runs last, before Hardware resolution and the persist:
// unlike every other section here it can fail (a mistyped power.reset is
// rejected rather than silently coerced), and a rejected config must not
// have Hardware resolved against it or get written back to disk.
func checkDefaultValue() error {
	needsPersist := applyJWTDefaults()

	applyCoreDefaults()
	applySerialDefaults()
	applyConsoleDefaults()
	applySSHDefaults()
	applyFirmwareDefaults()

	if migrateLegacyDataPaths() {
		needsPersist = true
	}

	applyUsbGadgetDefaults()
	applyTelemetryDefaults()
	applyAutoUpdateDefaults()

	if applyDiscoveryDefaults() {
		needsPersist = true
	}

	applyTimeSyncDefaults()
	applyNetworkDefaults()

	if err := applyPowerDefaults(); err != nil {
		return err
	}

	instance.Hardware = getHardware()

	// Persist generated values (the JWT secret) and the discovery migration
	// so neither has to be redone on the next boot.
	if needsPersist {
		persistConfig()
	}
	return nil
}

// applyPowerDefaults normalises power.reset: an absent key defaults to
// PowerResetAuto, and anything other than the three valid sentinels is
// rejected outright. This deliberately does not follow the silent-coercion
// pattern used elsewhere in this file (e.g. applyUsbGadgetDefaults' Ethernet
// switch): an operator who asks for "line" (reset only, error if unwired)
// must not have a typo silently degrade to "auto" or "cycle", which can
// substitute a power cycle — destructive to whatever the host OS was doing
// — for what they explicitly said should error instead.
func applyPowerDefaults() error {
	switch instance.Power.Reset {
	case "":
		instance.Power.Reset = PowerResetAuto
	case PowerResetAuto, PowerResetLine, PowerResetCycle:
		// operator's explicit, valid choice
	default:
		return fmt.Errorf("power.reset: invalid value %q (must be %q, %q or %q)",
			instance.Power.Reset, PowerResetAuto, PowerResetLine, PowerResetCycle)
	}
	return nil
}

// applyJWTDefaults seeds the signing secret and token lifetime. It reports
// whether it generated a secret, which the caller must persist.
func applyJWTDefaults() bool {
	generated := false

	if instance.JWT.SecretKey == "" {
		instance.JWT.SecretKey = generateRandomSecretKey()
		instance.JWT.RevokeTokensOnLogout = true
		generated = true
	}

	if instance.JWT.RefreshTokenDuration == 0 {
		instance.JWT.RefreshTokenDuration = 2678400
	}

	return generated
}

// applyCoreDefaults backfills the top-level service settings: authentication
// mode, protocol, certificate paths and the log destination.
//
// Stun is deliberately not defaulted here: empty means "LAN only", which
// is the right answer for a management controller, and filling it in
// would make an unset key indistinguishable from an opt-in.
func applyCoreDefaults() {
	if instance.Authentication == "" {
		instance.Authentication = "enable"
	}

	// An unset proto used to mean plaintext, because the https branch in
	// cmd/server tests for the literal string. That made a config written by
	// an older build — or hand-edited without the key — silently serve
	// Redfish over HTTP, where inventory tooling cannot see it at all (see
	// defaultConfig.Proto). Treat unset as "use the default" like every other
	// field here.
	if instance.Proto == "" {
		instance.Proto = defaultConfig.Proto
	}

	// Same reasoning for the certificate paths: unset must mean "the default
	// location", not "no TLS material", or an https config with no cert
	// section degrades to plaintext instead of provisioning itself.
	if instance.Cert.Crt == "" {
		instance.Cert.Crt = defaultConfig.Cert.Crt
	}
	if instance.Cert.Key == "" {
		instance.Cert.Key = defaultConfig.Cert.Key
	}

	// File logging is the default. Older builds persisted the previous "stdout"
	// default into server.yaml automatically, so treat that (and an unset value)
	// as "use the default" and adopt the rotating file log on upgrade. Set
	// logger.file to "console" to keep logging to stdout.
	if instance.Logger.File == "" || instance.Logger.File == "stdout" {
		instance.Logger.File = defaultConfig.Logger.File
	}
}

// applySerialDefaults applies serial defaults when not present in the config file.
func applySerialDefaults() {
	if instance.Serial.Device == "" {
		instance.Serial.Device = defaultConfig.Serial.Device
	}
	if instance.Serial.BaudRate == 0 {
		instance.Serial.BaudRate = defaultConfig.Serial.BaudRate
	}
	if instance.Serial.Parity == "" {
		instance.Serial.Parity = defaultConfig.Serial.Parity
	}
	if instance.Serial.DataBits == 0 {
		instance.Serial.DataBits = defaultConfig.Serial.DataBits
	}
	if instance.Serial.StopBits == 0 {
		instance.Serial.StopBits = defaultConfig.Serial.StopBits
	}
	if instance.Serial.FlowControl == "" {
		instance.Serial.FlowControl = defaultConfig.Serial.FlowControl
	}
	// Capture defaults: enabled unless explicitly switched off.
	if !viper.IsSet("serial.capture.enabled") {
		instance.Serial.Capture.Enabled = defaultConfig.Serial.Capture.Enabled
	}
	if instance.Serial.Capture.File == "" {
		instance.Serial.Capture.File = defaultConfig.Serial.Capture.File
	}
	if instance.Serial.Capture.MaxSizeKB <= 0 {
		instance.Serial.Capture.MaxSizeKB = defaultConfig.Serial.Capture.MaxSizeKB
	}
}

// applyConsoleDefaults normalises the console view. Normalised to one of the
// two sentinels rather than merely defaulted, so a stale or mistyped value in
// server.yaml lands on serial instead of being written back out and persisting
// forever.
func applyConsoleDefaults() {
	if instance.Console.PrimaryView != PrimaryViewHDMI {
		instance.Console.PrimaryView = PrimaryViewSerial
	}
}

// applySSHDefaults applies SSH defaults. Enabled and passwordAuth are
// default-true, so they go through viper.IsSet — a plain zero-value check
// cannot tell an operator's explicit false from an absent key. A config
// written before the in-process SSH server existed has no ssh section at all;
// seeding it keeps upgraded devices reachable over SSH exactly as they were
// when openssh was still in the image.
func applySSHDefaults() {
	if !viper.IsSet("ssh") {
		instance.SSH = defaultConfig.SSH
		return
	}

	if !viper.IsSet("ssh.enabled") {
		instance.SSH.Enabled = defaultConfig.SSH.Enabled
	}
	if !viper.IsSet("ssh.passwordAuth") {
		instance.SSH.PasswordAuth = defaultConfig.SSH.PasswordAuth
	}
	if instance.SSH.Port == 0 {
		instance.SSH.Port = defaultConfig.SSH.Port
	}
	if instance.SSH.HostKeyPath == "" {
		instance.SSH.HostKeyPath = defaultConfig.SSH.HostKeyPath
	}
	if instance.SSH.AuthorizedKeysPath == "" {
		instance.SSH.AuthorizedKeysPath = defaultConfig.SSH.AuthorizedKeysPath
	}
}

// applyFirmwareDefaults applies firmware defaults when not present in the
// config file. Configs written before the boot-image transport was retired
// carry imagePath, firmwareDir, mountPoint and the env-file keys; they no
// longer bind to any field and are dropped on the next Save().
func applyFirmwareDefaults() {
	if instance.Firmware.CapsulePath == "" {
		instance.Firmware.CapsulePath = defaultConfig.Firmware.CapsulePath
	}
	if instance.Firmware.CapsuleSizeMB <= 0 {
		instance.Firmware.CapsuleSizeMB = defaultConfig.Firmware.CapsuleSizeMB
	}
	if instance.Firmware.MediaDir == "" {
		instance.Firmware.MediaDir = defaultConfig.Firmware.MediaDir
	}
}

// migrateLegacyDataPaths rewrites pre-squashfs-layout paths. The /data
// partition no longer exists — every persistent path lives under
// /var/lib/nanokvm (the data partition the initramfs mounts) — so a config
// carried over from an old image or a restored backup would otherwise point
// the capsule volume, media dir and EEPROM snapshots at a dead mount.
// Rewritten in place and reported to the caller so the migration is persisted
// and runs once.
//
// This must run after applyFirmwareDefaults and applySSHDefaults: it rewrites
// the very fields they backfill.
func migrateLegacyDataPaths() bool {
	migratedDataPath := false
	for _, p := range []*string{
		&instance.Firmware.CapsulePath,
		&instance.Firmware.MediaDir,
		&instance.SSH.HostKeyPath,
		&instance.SSH.AuthorizedKeysPath,
	} {
		if rest, ok := strings.CutPrefix(*p, "/data/"); ok {
			*p = "/var/lib/nanokvm/" + rest
			migratedDataPath = true
		}
	}
	if migratedDataPath {
		log.Println("config: migrated legacy /data paths to /var/lib/nanokvm")
	}

	return migratedDataPath
}

// applyUsbGadgetDefaults applies USB gadget identity/path defaults when not
// present in the config file. The gadget config is now the sole source of
// truth for which USB functions are exposed (it replaced the /boot flags and
// the runtime state file), so the default-true toggles are seeded here, gated
// on viper.IsSet so an explicit false is distinguishable from an unset key — a
// plain zero-value check cannot tell them apart.
func applyUsbGadgetDefaults() {
	if instance.UsbGadget.VendorID == "" {
		instance.UsbGadget.VendorID = defaultConfig.UsbGadget.VendorID
	}
	if instance.UsbGadget.ProductID == "" {
		instance.UsbGadget.ProductID = defaultConfig.UsbGadget.ProductID
	}
	if instance.UsbGadget.SerialNumber == "" {
		instance.UsbGadget.SerialNumber = defaultConfig.UsbGadget.SerialNumber
	}
	if instance.UsbGadget.Manufacturer == "" {
		instance.UsbGadget.Manufacturer = defaultConfig.UsbGadget.Manufacturer
	}
	if instance.UsbGadget.Product == "" {
		instance.UsbGadget.Product = defaultConfig.UsbGadget.Product
	}
	if instance.UsbGadget.BmAttributes == "" {
		instance.UsbGadget.BmAttributes = defaultConfig.UsbGadget.BmAttributes
	}
	if instance.UsbGadget.MaxPower <= 0 {
		instance.UsbGadget.MaxPower = defaultConfig.UsbGadget.MaxPower
	}
	if instance.UsbGadget.OTGRolePath == "" {
		instance.UsbGadget.OTGRolePath = defaultConfig.UsbGadget.OTGRolePath
	}
	if instance.UsbGadget.PHYDevice == "" {
		instance.UsbGadget.PHYDevice = defaultConfig.UsbGadget.PHYDevice
	}

	// Function toggles. Each default-true bool is seeded only when its key is
	// absent, so an operator's explicit false is preserved. Ethernet is a
	// two-valued string ("off"/"ncm"); anything else (empty, invalid, or the
	// retired "ecm") falls back to the default.
	if !viper.IsSet("usbgadget.enabled") {
		instance.UsbGadget.Enabled = defaultConfig.UsbGadget.Enabled
	}
	if !viper.IsSet("usbgadget.hid") {
		instance.UsbGadget.HID = defaultConfig.UsbGadget.HID
	}
	if !viper.IsSet("usbgadget.wakeupOnWrite") {
		instance.UsbGadget.WakeupOnWrite = defaultConfig.UsbGadget.WakeupOnWrite
	}
	if !viper.IsSet("usbgadget.bindUDC") {
		instance.UsbGadget.BindUDC = defaultConfig.UsbGadget.BindUDC
	}
	if !viper.IsSet("usbgadget.disk") {
		instance.UsbGadget.Disk = defaultConfig.UsbGadget.Disk
	}
	switch instance.UsbGadget.Ethernet {
	case "off", "ncm":
		// keep the operator's value
	default:
		instance.UsbGadget.Ethernet = defaultConfig.UsbGadget.Ethernet
	}
}

// applyTelemetryDefaults backfills the service name and Prometheus scrape path.
func applyTelemetryDefaults() {
	if instance.Telemetry.ServiceName == "" {
		instance.Telemetry.ServiceName = defaultConfig.Telemetry.ServiceName
	}
	if instance.Telemetry.Prometheus.Path == "" {
		instance.Telemetry.Prometheus.Path = defaultConfig.Telemetry.Prometheus.Path
	}
}

// applyAutoUpdateDefaults clamps the poll interval so a zero/negative value
// can't spin the loop.
func applyAutoUpdateDefaults() {
	if instance.AutoUpdate.IntervalMinutes <= 0 {
		instance.AutoUpdate.IntervalMinutes = defaultConfig.AutoUpdate.IntervalMinutes
	}
}

// applyDiscoveryDefaults folds the legacy top-level mdns: block into
// discovery.mdns (see migrateDiscovery) before any defaulting happens, then
// applies the absent-section handling below to discovery.mdns, plus SSDP's own
// defaults. Only discovery.mdns gets defaulted: the legacy block is
// migration input, and defaults for a key that is about to be deleted
// would just be written back out as a resurrected mdns: block.
//
// It reports whether the file carried the legacy spelling, which the caller
// must persist.
//
// The absent-section check below must also test viper.IsSet("mdns"):
// a legacy-only file has no discovery.mdns key either, so testing
// !viper.IsSet("discovery.mdns") alone would treat a just-migrated
// block as absent and immediately stomp it back to hardcoded defaults —
// silently reverting an operator's non-default interface/hostname and,
// worse, flipping a deliberate `enabled: false` back to true.
//
// discoveryMDNSSet asks "is discovery.mdns itself explicitly set?" — not
// "does a discovery: block exist at all?" Those are different questions:
// a board can carry a discovery: block for SSDP alone (e.g.
// `discovery: {ssdp: {enabled: true}}`) with no discovery.mdns, while
// still configuring mDNS the legacy way. Gating the fold on the parent
// key's presence (viper.IsSet("discovery")) instead of the child's used
// to treat that shape as "explicit discovery, ignore legacy" — skipping
// the fold below in migrateDiscovery — and then delete the legacy block
// anyway, force-enabling mDNS on hardcoded defaults and erasing the
// operator's interface/hostname/disabled choice with no way back.
func applyDiscoveryDefaults() bool {
	discoveryMDNSSet := viper.IsSet("discovery.mdns")
	// legacyKeySet reads the parsed file, not the struct, so it still
	// answers "did this file use the old spelling?" after migrateDiscovery
	// has cleared the field — as do the per-key legacySet lookups below.
	legacyKeySet := viper.IsSet("mdns")
	migrateDiscovery(&instance, discoveryMDNSSet)
	if !discoveryMDNSSet && !legacyKeySet {
		instance.Discovery.MDNS = defaultConfig.Discovery.MDNS
	} else {
		// Enabled/IPv4/IPv6 default true, so a zero value is ambiguous with
		// an explicit false — the same problem the section-level check above
		// exists to prevent, just one level deeper. legacySet consults the
		// legacy mdns.<key> spelling only when discovery.mdns itself was not
		// explicitly set — the same condition that gated the fold in
		// migrateDiscovery above, so a key counts as "already set" here
		// exactly when the fold is what actually populated it. Once
		// discovery.mdns is explicitly set it wins in full, so a legacy
		// value must not count as "already set" for any of its own keys.
		// Skipping that distinction would reintroduce the CRITICAL-2 trap in
		// the other direction: a bare `discovery: {mdns: {interface: eth0}}`
		// would inherit whatever legacy's mdns.enabled happened to be (or
		// Go's false zero value if there were no legacy block at all),
		// silently leaving the responder off even though the operator wrote
		// no "enabled" key anywhere.
		legacySet := func(key string) bool {
			return !discoveryMDNSSet && viper.IsSet("mdns."+key)
		}
		if !viper.IsSet("discovery.mdns.enabled") && !legacySet("enabled") {
			instance.Discovery.MDNS.Enabled = defaultConfig.Discovery.MDNS.Enabled
		}
		if !viper.IsSet("discovery.mdns.ipv4") && !legacySet("ipv4") {
			instance.Discovery.MDNS.IPv4 = defaultConfig.Discovery.MDNS.IPv4
		}
		if !viper.IsSet("discovery.mdns.ipv6") && !legacySet("ipv6") {
			instance.Discovery.MDNS.IPv6 = defaultConfig.Discovery.MDNS.IPv6
		}
		if instance.Discovery.MDNS.Interface == "" &&
			!viper.IsSet("discovery.mdns.interface") && !legacySet("interface") {
			instance.Discovery.MDNS.Interface = defaultConfig.Discovery.MDNS.Interface
		}
	}
	if !viper.IsSet("discovery.ssdp") {
		instance.Discovery.SSDP = defaultConfig.Discovery.SSDP
	} else {
		if !viper.IsSet("discovery.ssdp.enabled") {
			instance.Discovery.SSDP.Enabled = defaultConfig.Discovery.SSDP.Enabled
		}
		if instance.Discovery.SSDP.MaxAge == 0 {
			instance.Discovery.SSDP.MaxAge = defaultConfig.Discovery.SSDP.MaxAge
		}
		// Empty SSDP interface inherits mDNS's rather than the default eth0
		// directly, so an operator who only overrides discovery.mdns.interface
		// gets both responders scoped to the same link without also having to
		// repeat it under ssdp.
		if instance.Discovery.SSDP.Interface == "" {
			instance.Discovery.SSDP.Interface = instance.Discovery.MDNS.Interface
		}
	}

	// Returning legacyKeySet asks the caller to rewrite the file once, on this
	// first boot after upgrade, so the legacy key actually leaves disk.
	// Migrating in memory only left both spellings in the file, and a file with
	// a discovery.mdns key makes migrateDiscovery skip the legacy block on the
	// next load — so the operator's values were read once and then lost.
	return legacyKeySet
}

// applyTimeSyncDefaults uses the same absent-section handling as mDNS (Enabled
// defaults true). When present, clamp the interval so a zero/negative value
// can't spin the loop.
func applyTimeSyncDefaults() {
	if !viper.IsSet("timesync") {
		instance.TimeSync = defaultConfig.TimeSync
	} else if instance.TimeSync.IntervalMinutes <= 0 {
		instance.TimeSync.IntervalMinutes = defaultConfig.TimeSync.IntervalMinutes
	}
}

// applyNetworkDefaults uses the same absent-section handling as mDNS (Enabled
// defaults true). When present, backfill the identity/mode fields so a partial
// section still has a usable interface name, mode and RHI address.
func applyNetworkDefaults() {
	if !viper.IsSet("network") {
		instance.Network = defaultConfig.Network
		return
	}

	if !viper.IsSet("network.enabled") {
		instance.Network.Enabled = defaultConfig.Network.Enabled
	}
	if instance.Network.Eth0.Name == "" {
		instance.Network.Eth0.Name = defaultConfig.Network.Eth0.Name
	}
	if instance.Network.Eth0.Mode == "" {
		instance.Network.Eth0.Mode = defaultConfig.Network.Eth0.Mode
	}
	if instance.Network.RHI.Interface == "" {
		instance.Network.RHI.Interface = defaultConfig.Network.RHI.Interface
	}
	if instance.Network.RHI.Address == "" {
		instance.Network.RHI.Address = defaultConfig.Network.RHI.Address
	}
	// An explicit empty lease disables the RHI DHCP server; only backfill
	// when the key is absent entirely.
	if instance.Network.RHI.Lease == "" && !viper.IsSet("network.rhi.lease") {
		instance.Network.RHI.Lease = defaultConfig.Network.RHI.Lease
	}
}

// migrateDiscovery folds a pre-discovery top-level `mdns:` block into
// discovery.mdns and then clears it, leaving discovery: as the single place
// the setting lives. Config files written before the SSDP responder existed
// use the old spelling.
//
// Clearing the field is what makes the migration final: the caller pairs it
// with a one-time rewrite (see checkDefaultValue), so the upgraded file has
// one spelling instead of two. Keeping both was actively harmful — a file
// with a discovery.mdns key takes the no-fold branch below, so the legacy
// values would be read on the migrating boot and dropped on every boot
// after it.
//
// mdnsSectionSet must mean "discovery.mdns is explicitly set", not "a
// discovery: block exists" — a caller that passes the latter folds nothing
// for a config with e.g. `discovery: {ssdp: {...}}` and no discovery.mdns,
// then still deletes the legacy block below, silently reverting the
// operator's mDNS settings to hardcoded defaults with no way to recover
// them. An explicit discovery.mdns block is authoritative even when a
// legacy block coexists with it: the legacy block is then stale text to
// delete, never a source of values.
func migrateDiscovery(c *Config, mdnsSectionSet bool) {
	if c.MDNS == nil {
		return
	}
	if !mdnsSectionSet {
		c.Discovery.MDNS = *c.MDNS
	}
	c.MDNS = nil
}
