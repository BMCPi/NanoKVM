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
		// Log to a rotating file (see server/logger). /var/log is tmpfs on
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
	Stun: "stun.l.google.com:19302",
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
		BIOSMode:      false,
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
	EfiVars: EfiVars{
		Enabled: true,
		// Read the store from the BMC's own i2c-slave-eeprom backing file. The
		// host (Raspberry Pi U-Boot, CONFIG_EFI_VARIABLE_I2C_STORE) writes the
		// variable blob into this EEPROM over I2C; the BMC reads/writes the same
		// bytes out-of-band through the slave device's backing file. This is
		// always safe — unlike raw /dev/i2c master access to 0x50, which would
		// address the BMC's *own* slave and cannot read the store.
		Path:      "/sys/bus/i2c/devices/0-1050/slave-eeprom",
		I2CBus:    -1, // disable the raw-master fallback
		I2CAddr:   0x50,
		PageSize:  64,
		StoreSize: 32768,
		// Durable mirror on the data partition (survives BMC reboots, unlike
		// the volatile i2c-slave-eeprom RAM buffer). Restored into the EEPROM
		// at startup and kept in sync with host/BMC writes.
		SnapshotPath: "/var/lib/nanokvm/efivars/store.bin",
	},
	UbootEnv: UbootEnv{
		Enabled: true,
		// The same EEPROM as the UEFI variable store (see EfiVars): the host's
		// CONFIG_ENV_IS_IN_EEPROM writes its env partition at 0x4000 of that
		// 24c256, and the BMC reads/writes the same bytes out-of-band through
		// the slave device's backing file.
		Path:     "/sys/bus/i2c/devices/0-1050/slave-eeprom",
		I2CBus:   -1, // disable the raw-master fallback
		I2CAddr:  0x50,
		PageSize: 64,
		Offset:   0x4000, // host CONFIG_ENV_OFFSET
		Size:     0x2000, // host CONFIG_ENV_SIZE
		// Durable mirror on the data partition; see EfiVars.SnapshotPath.
		SnapshotPath: "/var/lib/nanokvm/ubootenv/env.bin",
	},
	SMBIOS: SMBIOS{
		Enabled: true,
		// A third region of the same EEPROM (see EfiVars/UbootEnv): the
		// host's CONFIG_SMBIOS_I2C_STORE writes the tables it generates at
		// boot to 0x6000, and the BMC reads them out-of-band for inventory.
		Path:     "/sys/bus/i2c/devices/0-1050/slave-eeprom",
		I2CBus:   -1, // disable the raw-master fallback
		I2CAddr:  0x50,
		PageSize: 64,
		Offset:   0x6000, // host CONFIG_SMBIOS_I2C_STORE_OFFSET
		Size:     0x800,  // host CONFIG_SMBIOS_I2C_STORE_SIZE
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

	if instance.Stun == "" {
		instance.Stun = "stun.l.google.com:19302"
	}

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
		&instance.EfiVars.SnapshotPath,
		&instance.UbootEnv.SnapshotPath,
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

	normalizeEEPROMRegions()

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

// normalizeEEPROMRegions applies the defaults for the three stores that share
// the one 24c256, then holds their regions apart.
//
// efivars, ubootenv and smbios all address the same backing file, so a region
// that overruns its neighbour silently corrupts it. The clamps at the end also
// upgrade configs already persisted to /etc/kvm/server.yaml by an older build:
// a stale value is non-zero, so it survives the `<= 0` backfills and has to be
// corrected explicitly.
func normalizeEEPROMRegions() {
	// Apply EFI variable store defaults when not present in the config file.
	// When neither a path nor an explicit non-zero master bus is configured,
	// default to the BMC's own i2c-slave-eeprom backing file (see defaultConfig).
	// This also upgrades legacy configs that persisted the old "i2cBus: 0"
	// (raw master to 0x50), which cannot read the BMC's own slave EEPROM.
	if instance.EfiVars.Path == "" && instance.EfiVars.I2CBus == 0 {
		instance.EfiVars.Path = defaultConfig.EfiVars.Path
		instance.EfiVars.I2CBus = defaultConfig.EfiVars.I2CBus
	}
	if instance.EfiVars.I2CAddr == 0 {
		instance.EfiVars.I2CAddr = defaultConfig.EfiVars.I2CAddr
	}
	if instance.EfiVars.PageSize <= 0 {
		instance.EfiVars.PageSize = defaultConfig.EfiVars.PageSize
	}
	if instance.EfiVars.StoreSize <= 0 {
		instance.EfiVars.StoreSize = defaultConfig.EfiVars.StoreSize
	}
	// Backfill the durable snapshot path for configs written before it existed,
	// so persistence is enabled on upgrade without editing server.yaml.
	if instance.EfiVars.SnapshotPath == "" {
		instance.EfiVars.SnapshotPath = defaultConfig.EfiVars.SnapshotPath
	}

	// Apply U-Boot env store defaults, mirroring the EfiVars handling above:
	// the environment lives at an offset of the same EEPROM.
	if instance.UbootEnv.Path == "" && instance.UbootEnv.I2CBus == 0 {
		instance.UbootEnv.Path = defaultConfig.UbootEnv.Path
		instance.UbootEnv.I2CBus = defaultConfig.UbootEnv.I2CBus
	}
	if instance.UbootEnv.I2CAddr == 0 {
		instance.UbootEnv.I2CAddr = defaultConfig.UbootEnv.I2CAddr
	}
	if instance.UbootEnv.PageSize <= 0 {
		instance.UbootEnv.PageSize = defaultConfig.UbootEnv.PageSize
	}
	if instance.UbootEnv.Offset <= 0 {
		instance.UbootEnv.Offset = defaultConfig.UbootEnv.Offset
	}
	if instance.UbootEnv.Size <= 0 {
		instance.UbootEnv.Size = defaultConfig.UbootEnv.Size
	}
	if instance.UbootEnv.SnapshotPath == "" {
		instance.UbootEnv.SnapshotPath = defaultConfig.UbootEnv.SnapshotPath
	}

	// Apply SMBIOS store defaults, mirroring the handling above: the tables
	// live in a third region of the same EEPROM.
	if instance.SMBIOS.Path == "" && instance.SMBIOS.I2CBus == 0 {
		instance.SMBIOS.Path = defaultConfig.SMBIOS.Path
		instance.SMBIOS.I2CBus = defaultConfig.SMBIOS.I2CBus
	}
	if instance.SMBIOS.I2CAddr == 0 {
		instance.SMBIOS.I2CAddr = defaultConfig.SMBIOS.I2CAddr
	}
	if instance.SMBIOS.PageSize <= 0 {
		instance.SMBIOS.PageSize = defaultConfig.SMBIOS.PageSize
	}
	if instance.SMBIOS.Offset <= 0 {
		instance.SMBIOS.Offset = defaultConfig.SMBIOS.Offset
	}
	if instance.SMBIOS.Size <= 0 {
		instance.SMBIOS.Size = defaultConfig.SMBIOS.Size
	}

	// The three stores share one 24c256, so each region has to stop where the
	// next begins. Both clamps below also upgrade configs persisted to
	// /etc/kvm/server.yaml by an older build, which is why they cannot be
	// expressed as plain defaults — a stale value is non-zero and so survives
	// the `<= 0` backfills above.

	// The UEFI blob sits below the env partition and is otherwise bounded only
	// by the whole-chip storeSize, so it could grow into the environment.
	// Clamp it at the env offset — the BMC-side mirror of the cap U-Boot
	// applies at CONFIG_ENV_OFFSET. Upgrades configs holding the old
	// whole-chip storeSize (32768).
	if instance.UbootEnv.Enabled && instance.EfiVars.Path == instance.UbootEnv.Path &&
		instance.EfiVars.StoreSize > instance.UbootEnv.Offset {
		log.Printf("config: efiVars.storeSize %d overruns ubootEnv.offset %#x; clamping to %#x",
			instance.EfiVars.StoreSize, instance.UbootEnv.Offset, instance.UbootEnv.Offset)
		instance.EfiVars.StoreSize = instance.UbootEnv.Offset
	}

	// The env partition sits below the SMBIOS tables, and its size is not just
	// a bound but the CRC length: U-Boot checksums CONFIG_ENV_SIZE-4 bytes, so
	// a size that disagrees with the host makes every read fail with
	// "bad CRC, using default environment" — even though the bytes are intact.
	// Clamp it at the SMBIOS offset, which upgrades configs holding the old
	// 0x4000 env size (that value both mis-sizes the CRC and overruns the
	// tables at 0x6000).
	if instance.SMBIOS.Enabled && instance.UbootEnv.Enabled &&
		instance.UbootEnv.Path == instance.SMBIOS.Path &&
		instance.UbootEnv.Offset+instance.UbootEnv.Size > instance.SMBIOS.Offset {
		log.Printf("config: ubootEnv region %#x..%#x overruns smbios.offset %#x; "+
			"clamping size %#x -> %#x (a size disagreeing with the host's "+
			"CONFIG_ENV_SIZE makes U-Boot report a bad env CRC)",
			instance.UbootEnv.Offset, instance.UbootEnv.Offset+instance.UbootEnv.Size,
			instance.SMBIOS.Offset, instance.UbootEnv.Size,
			instance.SMBIOS.Offset-instance.UbootEnv.Offset)
		instance.UbootEnv.Size = instance.SMBIOS.Offset - instance.UbootEnv.Offset
	}

	log.Printf("config: eeprom layout %s — uefi [0x0,%#x) env [%#x,%#x) smbios [%#x,%#x)",
		instance.UbootEnv.Path,
		instance.EfiVars.StoreSize,
		instance.UbootEnv.Offset, instance.UbootEnv.Offset+instance.UbootEnv.Size,
		instance.SMBIOS.Offset, instance.SMBIOS.Offset+instance.SMBIOS.Size)
}
