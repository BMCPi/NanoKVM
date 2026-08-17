package config

import (
	"log"
	"strings"

	"github.com/spf13/viper"
)

var defaultConfig = &Config{
	Proto: "http",
	Port: Port{
		Http:  80,
		Https: 443,
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
		Enabled: true,
		Port:    623,
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
		ImageURL:      "https://github.com/tinkerbell-community/uboot-raspberrypi/releases/download/v2026.04-rc4.1/uboot-raspberrypi-2026.04-rc4.1.img.xz",
		ImagePath:     "/var/lib/nanokvm/firmware/uboot-rpi.img",
		SeedPath:      "/usr/share/rpi/uboot-rpi.img.xz",
		FirmwareDir:   "/var/lib/nanokvm/firmware/files",
		MountPoint:    "/var/lib/nanokvm/firmware/mnt",
		MachineEnv:    "/var/lib/nanokvm/firmware/files/machine.env",
		PersistentEnv: "/var/lib/nanokvm/firmware/files/persistent.env",
		OnceEnv:       "/var/lib/nanokvm/firmware/files/once.env",
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
		Ethernet:      "ecm", // "off"|"ecm"|"ncm"; matches usbgadget.EthernetECM
		Disk:          true,
		HID:           true,
		BIOSMode:      true, // boot-subclass HID: EDK2's UsbKbDxe only binds subclass-1 keyboards
		WakeupOnWrite: true,
		BindUDC:       true,
		UDCName:       "", // auto-detect (this board: 4340000.usb)
		OTGRolePath:   "/proc/cviusb/otg_role",
		PHYDevice:     "4340000.usb",
	},
	MDNS: MDNS{
		Enabled:   true,
		Interface: "eth0",
		IPv4:      true,
		IPv6:      true,
		Hostname:  "",
	},
	TimeSync: TimeSync{
		Enabled:         true,
		IntervalMinutes: 60,
	},
	Network: Network{
		Enabled: true,
		Eth0: InterfaceConfig{
			Name: "eth0",
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
		BIOS:            false,
	},
}

func checkDefaultValue() {
	needsPersist := false

	if instance.JWT.SecretKey == "" {
		instance.JWT.SecretKey = generateRandomSecretKey()
		instance.JWT.RevokeTokensOnLogout = true
		needsPersist = true
	}

	if instance.JWT.RefreshTokenDuration == 0 {
		instance.JWT.RefreshTokenDuration = 2678400
	}

	// Stun is deliberately not defaulted here: empty means "LAN only", which
	// is the right answer for a management controller, and filling it in
	// would make an unset key indistinguishable from an opt-in.

	if instance.Authentication == "" {
		instance.Authentication = "enable"
	}

	// File logging is the default. Older builds persisted the previous "stdout"
	// default into server.yaml automatically, so treat that (and an unset value)
	// as "use the default" and adopt the rotating file log on upgrade. Set
	// logger.file to "console" to keep logging to stdout.
	if instance.Logger.File == "" || instance.Logger.File == "stdout" {
		instance.Logger.File = defaultConfig.Logger.File
	}

	// Apply serial defaults when not present in the config file.
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

	// Console view. Normalised to one of the two sentinels rather than merely
	// defaulted, so a stale or mistyped value in server.yaml lands on serial
	// instead of being written back out and persisting forever.
	if instance.Console.PrimaryView != PrimaryViewHDMI {
		instance.Console.PrimaryView = PrimaryViewSerial
	}

	// Apply SSH defaults. Enabled and passwordAuth are default-true, so they
	// go through viper.IsSet — a plain zero-value check cannot tell an
	// operator's explicit false from an absent key. A config written before
	// the in-process SSH server existed has no ssh section at all; seeding it
	// keeps upgraded devices reachable over SSH exactly as they were when
	// openssh was still in the image.
	if !viper.IsSet("ssh") {
		instance.SSH = defaultConfig.SSH
	} else {
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

	// Apply firmware defaults when not present in the config file.
	if instance.Firmware.ImageURL == "" {
		instance.Firmware.ImageURL = defaultConfig.Firmware.ImageURL
	}
	if instance.Firmware.ImagePath == "" {
		instance.Firmware.ImagePath = defaultConfig.Firmware.ImagePath
	}
	if instance.Firmware.FirmwareDir == "" {
		instance.Firmware.FirmwareDir = defaultConfig.Firmware.FirmwareDir
	}
	if instance.Firmware.MountPoint == "" {
		instance.Firmware.MountPoint = defaultConfig.Firmware.MountPoint
	}
	if instance.Firmware.MachineEnv == "" {
		instance.Firmware.MachineEnv = defaultConfig.Firmware.MachineEnv
	}
	if instance.Firmware.PersistentEnv == "" {
		instance.Firmware.PersistentEnv = defaultConfig.Firmware.PersistentEnv
	}
	if instance.Firmware.OnceEnv == "" {
		instance.Firmware.OnceEnv = defaultConfig.Firmware.OnceEnv
	}
	if instance.Firmware.MediaDir == "" {
		instance.Firmware.MediaDir = defaultConfig.Firmware.MediaDir
	}

	// Migrate pre-squashfs-layout paths. The /data partition no longer exists
	// — every persistent path lives under /var/lib/nanokvm (the data
	// partition the initramfs mounts) — so a config carried over from an old
	// image or a restored backup would otherwise point the firmware image,
	// media dir and EEPROM snapshots at a dead mount. Rewritten in place and
	// persisted so the migration runs once.
	migratedDataPath := false
	for _, p := range []*string{
		&instance.Firmware.ImagePath,
		&instance.Firmware.FirmwareDir,
		&instance.Firmware.MountPoint,
		&instance.Firmware.MachineEnv,
		&instance.Firmware.PersistentEnv,
		&instance.Firmware.OnceEnv,
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
		needsPersist = true
	}

	// Apply USB gadget identity/path defaults when not present in the config
	// file. The gadget config is now the sole source of truth for which USB
	// functions are exposed (it replaced the /boot flags and the runtime state
	// file), so the default-true toggles are seeded here, gated on viper.IsSet
	// so an explicit false is distinguishable from an unset key — a plain
	// zero-value check cannot tell them apart.
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
	// three-valued string ("off"/"ecm"/"ncm"); anything else (empty or invalid)
	// falls back to the default.
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
	case "off", "ecm", "ncm":
		// keep the operator's value
	default:
		instance.UsbGadget.Ethernet = defaultConfig.UsbGadget.Ethernet
	}

	if instance.Telemetry.ServiceName == "" {
		instance.Telemetry.ServiceName = defaultConfig.Telemetry.ServiceName
	}
	if instance.Telemetry.Prometheus.Path == "" {
		instance.Telemetry.Prometheus.Path = defaultConfig.Telemetry.Prometheus.Path
	}

	if instance.AutoUpdate.IntervalMinutes <= 0 {
		instance.AutoUpdate.IntervalMinutes = defaultConfig.AutoUpdate.IntervalMinutes
	}

	// mDNS: the boolean fields default true, so a zero value is ambiguous with an
	// explicit false. When the whole section is absent (a config written before
	// mDNS existed), seed all defaults so upgraded devices keep advertising
	// <hostname>.local; when it is present, respect the operator's values.
	if !viper.IsSet("mdns") {
		instance.MDNS = defaultConfig.MDNS
	} else if instance.MDNS.Interface == "" && !viper.IsSet("mdns.interface") {
		instance.MDNS.Interface = defaultConfig.MDNS.Interface
	}

	// TimeSync: same absent-section handling (Enabled defaults true). When
	// present, clamp the interval so a zero/negative value can't spin the loop.
	if !viper.IsSet("timesync") {
		instance.TimeSync = defaultConfig.TimeSync
	} else if instance.TimeSync.IntervalMinutes <= 0 {
		instance.TimeSync.IntervalMinutes = defaultConfig.TimeSync.IntervalMinutes
	}

	// Network: same absent-section handling as mDNS (Enabled defaults true). When
	// present, backfill the identity/mode fields so a partial section still has a
	// usable interface name, mode and RHI address.
	if !viper.IsSet("network") {
		instance.Network = defaultConfig.Network
	} else {
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

	instance.Hardware = getHardware()

	// Persist the generated secret key so tokens survive server restarts.
	if needsPersist {
		persistConfig()
	}
}
