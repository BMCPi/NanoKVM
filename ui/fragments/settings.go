package fragments

// fragments_settings.go serves the settings dialog. Each panel has a GET that
// renders it from live state and one or more writes that apply a change and
// answer with that same panel re-rendered — so what the user sees after a
// write is always what was persisted, never what was submitted.

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	apivm "github.com/pi-bmc/nanokvm-app/api/vm"
	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/app/autoupdate"
	"github.com/pi-bmc/nanokvm-app/pkg/app/network"
	"github.com/pi-bmc/nanokvm-app/pkg/app/timesync"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"
	"github.com/pi-bmc/nanokvm-app/pkg/device/usbgadget"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/sysinfo"
	"github.com/pi-bmc/nanokvm-app/pkg/protocol/discovery"
	sshsvc "github.com/pi-bmc/nanokvm-app/pkg/protocol/ssh"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func settingsFragmentRoutes(g *gin.RouterGroup, h *handlers) {
	s := g.Group("/settings")

	s.GET("/general", func(c *gin.Context) {
		renderFragment(c, components.SettingsGeneralBody(generalModel()))
	})
	s.PATCH("/autoupdate", h.patchAutoUpdate)
	s.PATCH("/console", h.patchConsole)
	s.PATCH("/timesync", h.patchTimeSync)

	s.GET("/network", func(c *gin.Context) {
		renderFragment(c, components.SettingsNetworkBody(networkModel(h.log)))
	})
	s.PATCH("/network", h.patchNetwork)
	s.PATCH("/mdns", h.patchMDNS)
	s.GET("/network/status", func(c *gin.Context) {
		renderFragment(c, components.SettingsNetworkStatusRows(networkStatus(h.log)))
	})

	s.GET("/serial", func(c *gin.Context) {
		renderFragment(c, components.SettingsSerialBody(serialModel()))
	})
	s.PATCH("/serial", h.patchSerial)

	s.GET("/hardware", func(c *gin.Context) {
		renderFragment(c, components.SettingsHardwareBody(hardwareModel(h.d)))
	})
	s.POST("/hardware", h.postHardware)
	s.PATCH("/gadget", h.patchGadget)
	s.PATCH("/power", h.patchPower)

	s.GET("/access", func(c *gin.Context) {
		renderFragment(c, components.SettingsAccessBody(h.accessModel()))
	})
	s.POST("/access/ssh", h.postSSHEnabled)
	s.POST("/access/ssh/keys", h.postSSHKeys)
	s.POST("/access/tls", h.postTLS)
	s.PATCH("/access/ports", h.patchWebPorts)
	s.PATCH("/access/security", h.patchLoginSecurity)
	s.PATCH("/access/protocols", h.patchProtocols)

	s.GET("/telemetry", func(c *gin.Context) {
		renderFragment(c, components.SettingsTelemetryBody(telemetryModel()))
	})
	s.PATCH("/telemetry", h.patchTelemetry)

	s.GET("/advanced", func(c *gin.Context) {
		renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
	})
	s.PATCH("/logging", h.patchLogging)
	s.PATCH("/webrtc", h.patchWebRTC)
	s.PATCH("/storage", h.patchStorage)
	s.POST("/reboot", h.postReboot)
}

// atoiClamp parses a submitted number. A missing, unparseable or out-of-range
// value keeps current rather than resetting the setting: these forms submit on
// every change, so a half-typed number must not be able to clobber a good one.
// floor is re-checked here and not only in the input's min attribute, because a
// hand-crafted POST never sees that attribute.
func atoiClamp(s string, current, floor int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < floor {
		return current
	}
	return n
}

// restartNote is the description every "applies at startup" toast carries, so
// the UI never implies a change took effect that has not.
const restartNote = "Saved. Applies after the BMC restarts."

// checked reads a checkbox out of a submitted form. An unchecked box is
// omitted by the browser entirely, which is the HTML way of saying false —
// safe here because each form always posts every switch it owns.
func checked(c *gin.Context, name string) bool {
	return c.PostForm(name) != ""
}

// ── General ─────────────────────────────────────────────────────────────

func generalModel() components.SettingsGeneral {
	conf := config.GetInstance()
	au := conf.AutoUpdate
	ts := conf.TimeSync
	return components.SettingsGeneral{
		AppVersion:            application.CurrentVersion(),
		ImageVersion:          sysinfo.ImageVersion(),
		Hardware:              conf.Hardware.Version.String(),
		AutoUpdateEnabled:     au.Enabled,
		AutoUpdateInterval:    strconv.Itoa(au.IntervalMinutes),
		AutoUpdateApplication: au.Application,
		ConsolePrimaryView:    conf.Console.PrimaryView,
		TimeSyncEnabled:       ts.Enabled,
		TimeSyncServers:       strings.Join(ts.Servers, ", "),
		TimeSyncInterval:      strconv.Itoa(ts.IntervalMinutes),
	}
}

// patchTimeSync re-reads the whole sync loop via Restart, which stops the
// previous loop before starting the new one — otherwise turning timesync off
// would leave the previous loop running and still touching the clock.
func (h *handlers) patchTimeSync(c *gin.Context) {
	ts := &config.GetInstance().TimeSync
	ts.Enabled = checked(c, "enabled")
	ts.Servers = splitList(c.PostForm("servers"))
	ts.IntervalMinutes = atoiClamp(c.PostForm("intervalMinutes"), ts.IntervalMinutes, 1)

	config.Save()
	// The process context, not the request's: the sync loop outlives this
	// call and must only stop at shutdown.
	timesync.Restart(h.d.Ctx)

	hxToast(c, "success", "Settings saved", "Clock synchronisation reconfigured.")
	renderFragment(c, components.SettingsGeneralBody(generalModel()))
}

// patchConsole persists which view the dashboard opens on.
//
// Nothing is restarted and no session is touched: the setting only picks the
// tab the next page load lands on, and both views stay reachable either way.
// The toast says so, because a settings write that appears to do nothing to
// the current screen is otherwise indistinguishable from one that failed.
func (h *handlers) patchConsole(c *gin.Context) {
	view := config.PrimaryViewSerial
	if c.PostForm("primaryView") == config.PrimaryViewHDMI {
		view = config.PrimaryViewHDMI
	}
	config.GetInstance().Console.PrimaryView = view

	config.Save()

	hxToast(c, "success", "Settings saved", "The dashboard will open on this view from the next page load.")
	renderFragment(c, components.SettingsGeneralBody(generalModel()))
}

func (h *handlers) patchAutoUpdate(c *gin.Context) {
	au := &config.GetInstance().AutoUpdate
	au.Enabled = checked(c, "enabled")
	au.Application = checked(c, "application")
	if n, err := strconv.Atoi(c.PostForm("intervalMinutes")); err == nil && n > 0 {
		au.IntervalMinutes = n
	}

	config.Save()
	// The process context, not the request's: the ticker outlives this call
	// and must only stop at shutdown.
	autoupdate.Restart(h.d.Ctx) // re-reads config; cancels an existing ticker

	hxToast(c, "success", "Settings saved", "Automatic update settings applied.")
	renderFragment(c, components.SettingsGeneralBody(generalModel()))
}

// ── Network ─────────────────────────────────────────────────────────────

func networkModel(log *slog.Logger) components.SettingsNetwork {
	n := config.GetInstance().Network
	// Discovery.MDNS, not the top-level MDNS field: the latter is a
	// migration landing spot only (see migrateDiscovery in pkg/config) and
	// pkg/discovery never reads it, so rendering from it would show settings
	// that are not actually in effect.
	md := config.GetInstance().Discovery.MDNS
	return components.SettingsNetwork{
		Enabled:       n.Enabled,
		Mode:          strings.ToLower(n.Eth0.Mode),
		MAC:           n.Eth0.MAC,
		Address:       n.Eth0.Address,
		Gateway:       n.Eth0.Gateway,
		DNS:           strings.Join(n.Eth0.DNS, ", "),
		RHIAddress:    n.RHI.Address,
		RHILease:      n.RHI.Lease,
		MDNSEnabled:   md.Enabled,
		MDNSHostname:  md.Hostname,
		MDNSInterface: md.Interface,
		MDNSIPv4:      md.IPv4,
		MDNSIPv6:      md.IPv6,
		Status:        networkStatus(log),
	}
}

// patchMDNS restarts only the responder, leaving addressing alone — which is
// why it is a separate form from the batched eth0 config whose Save warns it
// may drop the session.
//
// It writes Discovery.MDNS, the block pkg/discovery actually starts from —
// not the legacy top-level MDNS field. Writing the legacy field here would
// make the form a silent no-op: discovery.Restart() below would restart from
// the unchanged Discovery.MDNS, and a subsequent config.Save() would write a
// discovery: key that makes migrateDiscovery skip the legacy block on the
// next load, permanently discarding whatever the form "saved".
func (h *handlers) patchMDNS(c *gin.Context) {
	md := &config.GetInstance().Discovery.MDNS
	md.Enabled = checked(c, "enabled")
	md.Hostname = strings.TrimSpace(c.PostForm("hostname"))
	md.Interface = strings.TrimSpace(c.PostForm("interface"))
	md.IPv4 = checked(c, "ipv4")
	md.IPv6 = checked(c, "ipv6")

	config.Save()
	// The process context, not the request's: the responders outlive this
	// call and must only stop at shutdown.
	discovery.Restart(h.d.Ctx)

	hxToast(c, "success", "Settings saved", "Discovery responder restarted.")
	renderFragment(c, components.SettingsNetworkBody(networkModel(h.log)))
}

// ── Serial ──────────────────────────────────────────────────────────────

func serialModel() components.SettingsSerial {
	s := config.GetInstance().Serial
	// Resolved, not stored: the broker picks the gadget's ttyGS over
	// serial.device at open time, so this form's Device field is only the
	// console port while the gadget console is off. Showing both is what keeps
	// the form from quietly presenting a UART nobody is reading.
	consoleDevice, fromGadget := serial.ConsoleDeviceInfo()
	return components.SettingsSerial{
		Device:              s.Device,
		BaudRate:            strconv.Itoa(s.BaudRate),
		DataBits:            strconv.Itoa(s.DataBits),
		Parity:              strings.ToLower(s.Parity),
		StopBits:            strconv.Itoa(s.StopBits),
		FlowControl:         strings.ToLower(s.FlowControl),
		ConsoleDevice:       consoleDevice,
		GadgetConsoleActive: fromGadget,
		CaptureEnabled:      s.Capture.Enabled,
		CaptureFile:         s.Capture.File,
		CaptureMaxKB:        strconv.Itoa(s.Capture.MaxSizeKB),
	}
}

// patchSerial writes the port config and re-opens the port so the new framing
// takes effect now rather than at the next reboot. That disconnects live
// console sessions — the form's hx-confirm says so before it happens.
//
// The device path is not validated here: an operator moving the console to a
// different tty is a legitimate change, and the broker already reports an open
// failure through the console's own connection status.
func (h *handlers) patchSerial(c *gin.Context) {
	s := &config.GetInstance().Serial
	if dev := strings.TrimSpace(c.PostForm("device")); dev != "" {
		s.Device = dev
	}
	s.BaudRate = atoiClamp(c.PostForm("baudRate"), s.BaudRate, 50)
	s.DataBits = atoiClamp(c.PostForm("dataBits"), s.DataBits, 5)
	s.StopBits = atoiClamp(c.PostForm("stopBits"), s.StopBits, 1)
	if p := strings.ToLower(strings.TrimSpace(c.PostForm("parity"))); p != "" {
		s.Parity = p
	}
	if f := strings.ToLower(strings.TrimSpace(c.PostForm("flowControl"))); f != "" {
		s.FlowControl = f
	}
	s.Capture.Enabled = checked(c, "captureEnabled")
	s.Capture.MaxSizeKB = atoiClamp(c.PostForm("captureMaxKB"), s.Capture.MaxSizeKB, 16)

	config.Save()
	serial.Restart()

	h.log.InfoContext(c.Request.Context(), "serial: settings updated via ui",
		slog.String("device", s.Device), slog.Int("baudRate", s.BaudRate),
		slog.Int("dataBits", s.DataBits), slog.String("parity", s.Parity), slog.Int("stopBits", s.StopBits))
	hxToast(c, "success", "Serial settings saved", "The port was re-opened; reconnect the console.")
	renderFragment(c, components.SettingsSerialBody(serialModel()))
}

// networkStatus reports what the interfaces actually came up with, which is
// not necessarily what the form above asked for — a DHCP lease, a kernel MAC,
// or a link that failed to come up all show here.
func networkStatus(log *slog.Logger) components.SettingsNetworkStatus {
	cfg := config.GetInstance().Network
	st := components.SettingsNetworkStatus{
		Mode: strings.ToUpper(strings.ToLower(cfg.Eth0.Mode)),
		MAC:  cfg.Eth0.MAC,
	}
	if st.MAC == "" {
		st.MAC = "kernel default"
	}

	for _, ip := range sysinfo.IPs(log) {
		switch {
		case strings.HasPrefix(ip.Addr, "169.254."):
			if st.RHI == "" {
				st.RHI = ip.Addr
			}
		case strings.HasPrefix(ip.Addr, "127."):
		default:
			if st.IP == "" {
				st.IP, st.Interface = ip.Addr, ip.Name
			}
		}
	}
	if name, ok := discovery.Advertised(); ok {
		st.MDNS = name
	}
	return st
}

// patchNetwork validates against a copy so a rejected value leaves the live
// config untouched, then restarts the manager to apply the new addressing
// without a process restart — the same contract as PATCH /api/network/settings.
func (h *handlers) patchNetwork(c *gin.Context) {
	conf := config.GetInstance()
	next := conf.Network

	next.Enabled = checked(c, "enabled")
	next.Eth0.Mode = strings.ToLower(c.PostForm("mode"))
	next.Eth0.MAC = strings.TrimSpace(c.PostForm("mac"))
	next.Eth0.Address = strings.TrimSpace(c.PostForm("address"))
	next.Eth0.Gateway = strings.TrimSpace(c.PostForm("gateway"))
	next.Eth0.DNS = splitList(c.PostForm("dns"))
	next.RHI.Address = strings.TrimSpace(c.PostForm("rhiAddress"))
	next.RHI.Lease = strings.TrimSpace(c.PostForm("rhiLease"))

	if err := network.Validate(&next); err != nil {
		// Re-render from the untouched config so the form snaps back to the
		// last good values instead of holding the rejected ones.
		hxToast(c, "error", "Network settings rejected", err.Error())
		renderFragment(c, components.SettingsNetworkBody(networkModel(h.log)))
		return
	}

	conf.Network = next
	config.Save()
	network.Restart()

	h.log.InfoContext(c.Request.Context(), "network: settings updated via ui",
		slog.String("eth0Mode", next.Eth0.Mode), slog.Bool("enabled", next.Enabled))
	hxToast(c, "success", "Network settings saved", "Addressing re-applied.")
	renderFragment(c, components.SettingsNetworkBody(networkModel(h.log)))
}

// splitList parses the comma/space separated DNS field.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// ── Hardware ────────────────────────────────────────────────────────────

func hardwareModel(d *deps.Deps) components.SettingsHardware {
	st := usbgadget.Get().State()
	g := config.GetInstance().UsbGadget
	m := components.SettingsHardware{
		// Ethernet and Disk come from the live gadget, the rest from config:
		// the first pair is reconciled at runtime and can differ from what was
		// asked for, the rest is only read when the tree is built at boot.
		USBNetwork: st.Ethernet != usbgadget.EthernetOff,
		USBDisk:    st.Disk,
		MediaState: "Not inserted",

		// From the live gadget for the same reason Ethernet and Disk are. The
		// console device is resolved, not stored: the broker picks the
		// gadget's ttyGS over serial.device at open time, so this is the only
		// place that says which port the terminal and SOL are actually on.
		USBSerialConsole: st.SerialConsole,
		ConsoleDevice:    serial.ConsoleDevice(),

		GadgetEnabled: g.Enabled,
		HID:           g.HID,
		BIOSMode:      g.BIOSMode,
		WakeupOnWrite: g.WakeupOnWrite,
		VendorID:      g.VendorID,
		ProductID:     g.ProductID,
		Manufacturer:  g.Manufacturer,
		Product:       g.Product,
		SerialNumber:  g.SerialNumber,

		PowerLegacyMode: config.GetInstance().Power.LegacyMode,
	}
	if d.Firmware.GetVirtualMediaState().Inserted {
		m.MediaState = "Inserted"
	}
	return m
}

// patchGadget persists the descriptor and HID settings. Nothing is applied
// live: pkg/usbgadget exposes runtime setters for Ethernet and Disk only, and
// everything here is consumed by build() when the configfs tree is created at
// startup. Re-enumerating the device under a running host mid-session would
// also drop the keyboard the operator may be using to watch it happen.
func (h *handlers) patchGadget(c *gin.Context) {
	g := &config.GetInstance().UsbGadget
	g.Enabled = checked(c, "enabled")
	g.HID = checked(c, "hid")
	g.BIOSMode = checked(c, "biosMode")
	g.WakeupOnWrite = checked(c, "wakeupOnWrite")
	if v := strings.TrimSpace(c.PostForm("vendorID")); v != "" {
		g.VendorID = v
	}
	if v := strings.TrimSpace(c.PostForm("productID")); v != "" {
		g.ProductID = v
	}
	g.Manufacturer = strings.TrimSpace(c.PostForm("manufacturer"))
	g.Product = strings.TrimSpace(c.PostForm("product"))
	g.SerialNumber = strings.TrimSpace(c.PostForm("serialNumber"))

	config.Save()

	hxToast(c, "success", "USB settings saved", restartNote)
	renderFragment(c, components.SettingsHardwareBody(hardwareModel(h.d)))
}

// patchPower switches how the power pin is driven. Not applied live:
// power.NewController snapshots legacyMode at construction (pkg/power/power.go
// — "legacyMode and the GPIO pins are fixed at construction from config"), and
// the controller is built once in main and threaded through deps, so the
// running instance keeps the old mode until the process restarts.
func (h *handlers) patchPower(c *gin.Context) {
	config.GetInstance().Power.LegacyMode = checked(c, "legacyMode")
	config.Save()

	hxToast(c, "success", "Settings saved", restartNote)
	renderFragment(c, components.SettingsHardwareBody(hardwareModel(h.d)))
}

// postHardware sets the gadget functions to the submitted state rather than
// toggling them: the form carries the state the user wants, and the gadget
// package no-ops when a value already matches.
func (h *handlers) postHardware(c *gin.Context) {
	gadget := usbgadget.Get()

	mode := usbgadget.EthernetOff
	if checked(c, "network") {
		mode = usbgadget.EthernetNCM
	}
	if err := gadget.SetEthernet(mode); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: set ethernet failed", slog.String("mode", mode), slog.Any("err", err))
		hxToast(c, "error", "USB Ethernet unchanged", err.Error())
	}
	if err := gadget.SetDisk(checked(c, "disk")); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: set disk failed", slog.Any("err", err))
		hxToast(c, "error", "USB Mass Storage unchanged", err.Error())
	}

	// The serial console is the one function that also owns which port the
	// broker opens, so a change here has to reach the broker too: it snapshots
	// the device at open time and would otherwise stay on the old one until
	// something else restarted it. Restarted only on an actual change — this
	// form posts on every switch in the panel, and Restart drops live console
	// sessions.
	wantConsole := checked(c, "serialConsole")
	consoleChanged := wantConsole != gadget.State().SerialConsole
	if err := gadget.SetSerialConsole(wantConsole); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: set serial console failed", slog.Bool("on", wantConsole), slog.Any("err", err))
		hxToast(c, "error", "USB Serial Console unchanged", err.Error())
	} else if consoleChanged {
		serial.Restart()
		h.log.InfoContext(c.Request.Context(), "usbgadget: serial console toggled via ui",
			slog.Bool("on", wantConsole), slog.String("device", serial.ConsoleDevice()))
		hxToast(c, "success", "USB Serial Console updated",
			"The USB device re-enumerated and the console was re-opened on "+serial.ConsoleDevice()+".")
	}

	renderFragment(c, components.SettingsHardwareBody(hardwareModel(h.d)))
}

// ── Access ──────────────────────────────────────────────────────────────

func (h *handlers) accessModel() components.SettingsAccess {
	keys, err := sshsvc.ReadAuthorizedKeys()
	if err != nil {
		h.log.Warn("ui: read authorized keys", slog.Any("err", err))
	}
	conf := config.GetInstance()
	return components.SettingsAccess{
		SSHEnabled:      conf.SSH.Enabled,
		SSHKeys:         keys,
		SSHPort:         strconv.Itoa(conf.SSH.Port),
		SSHPasswordAuth: conf.SSH.PasswordAuth,

		TLSEnabled: conf.Proto == "https",
		HTTPPort:   strconv.Itoa(conf.Port.HTTP),
		HTTPSPort:  strconv.Itoa(conf.Port.HTTPS),

		LoginMaxFailures:     strconv.Itoa(conf.Security.LoginMaxFailures),
		LoginLockoutDuration: strconv.Itoa(conf.Security.LoginLockoutDuration),

		SessionDuration:    strconv.FormatUint(conf.JWT.RefreshTokenDuration, 10),
		RevokeTokensLogout: conf.JWT.RevokeTokensOnLogout,

		IPMIEnabled:    conf.IPMI.Enabled,
		IPMIPort:       strconv.Itoa(conf.IPMI.Port),
		RedfishEnabled: conf.Redfish.Enabled,
	}
}

// postSSHEnabled owns the whole SSH form — the switch, the port and the
// password-auth policy — because all three are applied by the same restart.
//
// A failure (the port is taken, say) rolls the whole submission back, and
// because the response is the panel re-read from config, the controls snap back
// on their own rather than showing a state the listener never reached.
func (h *handlers) postSSHEnabled(c *gin.Context) {
	conf := config.GetInstance()
	previous := conf.SSH

	conf.SSH.Enabled = checked(c, "enabled")
	conf.SSH.PasswordAuth = checked(c, "passwordAuth")
	conf.SSH.Port = atoiClamp(c.PostForm("port"), conf.SSH.Port, 1)

	if err := sshsvc.Restart(); err != nil {
		conf.SSH = previous
		h.log.ErrorContext(c.Request.Context(), "ui: apply SSH state", slog.Any("err", err))
		hxToast(c, "error", "SSH unchanged", err.Error())
		// Put the listener back the way it was; the restart above left it
		// stopped, and the rolled-back config is what it should be running.
		if rerr := sshsvc.Restart(); rerr != nil {
			h.log.ErrorContext(c.Request.Context(), "ui: restore SSH listener", slog.Any("err", rerr))
		}
	} else if previous != conf.SSH {
		config.Save()
		hxToast(c, "success", "SSH settings saved", "")
	}

	renderFragment(c, components.SettingsAccessBody(h.accessModel()))
}

// patchWebPorts records the listener ports. Not applied live: the HTTP server
// binds them in main at startup, so changing one here without a restart would
// leave the UI claiming a port nothing is listening on.
func (h *handlers) patchWebPorts(c *gin.Context) {
	p := &config.GetInstance().Port
	p.HTTP = atoiClamp(c.PostForm("httpPort"), p.HTTP, 1)
	p.HTTPS = atoiClamp(c.PostForm("httpsPort"), p.HTTPS, 1)

	config.Save()

	hxToast(c, "success", "Settings saved", restartNote)
	renderFragment(c, components.SettingsAccessBody(h.accessModel()))
}

// patchLoginSecurity applies immediately: pkg/auth/brute_force.go and
// pkg/middleware/jwt.go read these out of config on every attempt rather than
// caching them, so there is nothing to restart.
func (h *handlers) patchLoginSecurity(c *gin.Context) {
	conf := config.GetInstance()
	conf.Security.LoginMaxFailures = atoiClamp(c.PostForm("maxFailures"), conf.Security.LoginMaxFailures, 1)
	// Zero is meaningful here — it disables lockout — so the floor is 0, not 1.
	conf.Security.LoginLockoutDuration = atoiClamp(c.PostForm("lockoutDuration"), conf.Security.LoginLockoutDuration, 0)
	if n := atoiClamp(c.PostForm("sessionDuration"), int(conf.JWT.RefreshTokenDuration), 60); n > 0 { //nolint:gosec // G115: this conversion only feeds atoiClamp's unchanged-value fallback, used solely as the n > 0 comparand right here; if RefreshTokenDuration (an operator-supplied seconds count, see pkg/middleware/jwt.go) were ever large enough to wrap negative, n > 0 discards it instead of writing it back
		conf.JWT.RefreshTokenDuration = uint64(n)
	}
	conf.JWT.RevokeTokensOnLogout = checked(c, "revokeOnLogout")

	config.Save()

	hxToast(c, "success", "Settings saved", "Login policy applied to new sessions.")
	renderFragment(c, components.SettingsAccessBody(h.accessModel()))
}

// patchProtocols records the out-of-band interfaces. Neither is applied live:
// the IPMI listener is started once in main, and the Redfish routes are
// registered on the router at startup.
func (h *handlers) patchProtocols(c *gin.Context) {
	conf := config.GetInstance()
	conf.IPMI.Enabled = checked(c, "ipmiEnabled")
	conf.IPMI.Port = atoiClamp(c.PostForm("ipmiPort"), conf.IPMI.Port, 1)
	conf.Redfish.Enabled = checked(c, "redfishEnabled")

	config.Save()

	hxToast(c, "success", "Settings saved", restartNote)
	renderFragment(c, components.SettingsAccessBody(h.accessModel()))
}

// ── Telemetry ───────────────────────────────────────────────────────────

func telemetryModel() components.SettingsTelemetry {
	t := config.GetInstance().Telemetry
	return components.SettingsTelemetry{
		Enabled:           t.Enabled,
		ServiceName:       t.ServiceName,
		PrometheusEnabled: t.Prometheus.Enabled,
		PrometheusPath:    t.Prometheus.Path,
		OTLPEndpoint:      t.OTLP.Endpoint,
		OTLPInsecure:      t.OTLP.Insecure,
	}
}

// patchTelemetry records collection settings. telemetry.Init wires the
// meter/tracer providers and the scrape handler once at startup, so none of
// this takes effect until the process restarts — including the master switch,
// which is why the toast says so rather than implying the charts will fill in.
func (h *handlers) patchTelemetry(c *gin.Context) {
	t := &config.GetInstance().Telemetry
	t.Enabled = checked(c, "enabled")
	t.ServiceName = strings.TrimSpace(c.PostForm("serviceName"))
	t.Prometheus.Enabled = checked(c, "prometheusEnabled")
	if p := strings.TrimSpace(c.PostForm("prometheusPath")); p != "" {
		t.Prometheus.Path = p
	}
	t.OTLP.Endpoint = strings.TrimSpace(c.PostForm("otlpEndpoint"))
	t.OTLP.Insecure = checked(c, "otlpInsecure")

	config.Save()

	hxToast(c, "success", "Settings saved", restartNote)
	renderFragment(c, components.SettingsTelemetryBody(telemetryModel()))
}

func (h *handlers) postSSHKeys(c *gin.Context) {
	keys := c.PostForm("sshKey")
	if err := sshsvc.ValidateSSHKey(keys); err != nil {
		hxToast(c, "error", "Keys not saved", err.Error())
		renderFragment(c, components.SettingsAccessBody(h.accessModel()))
		return
	}
	if err := sshsvc.WriteAuthorizedKeys(keys); err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: write authorized keys", slog.Any("err", err))
		hxToast(c, "error", "Keys not saved", err.Error())
		renderFragment(c, components.SettingsAccessBody(h.accessModel()))
		return
	}

	hxToast(c, "success", "Authorized keys saved", "")
	renderFragment(c, components.SettingsAccessBody(h.accessModel()))
}

// postTLS rewrites the persisted proto and restarts the service, which is the
// only way the new listener takes effect. The response tells htmx to reload
// the page on the new scheme once the service is back.
func (h *handlers) postTLS(c *gin.Context) {
	enable := checked(c, "enabled")

	var err error
	if enable {
		err = apivm.EnableTLS(h.log)
	} else {
		err = apivm.DisableTLS()
	}
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "ui: set TLS", slog.Any("err", err))
		hxToast(c, "error", "TLS unchanged", err.Error())
		renderFragment(c, components.SettingsAccessBody(h.accessModel()))
		return
	}

	scheme := "http"
	if enable {
		scheme = "https"
	}
	hxToast(c, "info", "Restarting service", "Reconnect over "+scheme+".")
	renderFragment(c, components.SettingsAccessBody(h.accessModel()))

	// Exit after the response has flushed and let init respawn us.
	//
	// DELIBERATELY DETACHED: this outlives the request. If RestartService ever
	// takes a context.Context, give it context.Background() — never c or
	// c.Request.Context(). The request completes the moment this handler
	// returns, so a request-scoped context would cancel the restart before it
	// happened, and the UI would report a scheme change that never occurred.
	// Same hazard as postReboot below.
	go application.RestartService(h.log)
}

// ── Advanced ────────────────────────────────────────────────────────────

func advancedModel() components.SettingsAdvanced {
	conf := config.GetInstance()
	return components.SettingsAdvanced{
		DeviceKey: sysinfo.DeviceKey(),

		LogLevel: strings.ToLower(conf.Logger.Level),
		LogFile:  conf.Logger.File,

		Stun:     conf.Stun,
		TurnAddr: conf.Turn.TurnAddr,
		TurnUser: conf.Turn.TurnUser,
		TurnCred: conf.Turn.TurnCred,

		MediaDir: conf.Firmware.MediaDir,
	}
}

// patchLogging applies the level immediately via logger.SetLevel, which moves
// the shared slog.LevelVar every logging path — native slog call sites and the
// bridged logrus ones alike — filters on. (logrus.SetLevel would not do:
// the bridge pins logrus wide open and filters in the slog handler, so setting
// it would pre-filter bridged entries while leaving slog call sites untouched.)
// An unrecognised level is rejected rather than defaulting, because silently
// logging at a different level than the one displayed is worse than an error.
func (h *handlers) patchLogging(c *gin.Context) {
	lvl := strings.ToLower(strings.TrimSpace(c.PostForm("level")))
	if err := logger.SetLevel(lvl); err != nil {
		hxToast(c, "error", "Log level unchanged", "Unrecognised level "+lvl+".")
		renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
		return
	}

	config.GetInstance().Logger.Level = lvl
	config.Save()

	hxToast(c, "success", "Settings saved", "Now logging at "+lvl+".")
	renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
}

// patchWebRTC records the ICE servers. These are read when a console session
// negotiates, and handed to the browser as the page's ice-servers data
// attribute, so a change reaches the next session without a restart — no
// running session is renegotiated.
func (h *handlers) patchWebRTC(c *gin.Context) {
	conf := config.GetInstance()
	conf.Stun = strings.TrimSpace(c.PostForm("stun"))
	conf.Turn.TurnAddr = strings.TrimSpace(c.PostForm("turnAddr"))
	conf.Turn.TurnUser = strings.TrimSpace(c.PostForm("turnUser"))
	conf.Turn.TurnCred = strings.TrimSpace(c.PostForm("turnCred"))

	config.Save()

	hxToast(c, "success", "Settings saved", "Applies to the next console session.")
	renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
}

// patchStorage moves where uploaded ISOs are kept. Existing images are not
// relocated — the directory is read per operation, so the old ones simply stop
// being listed, and saying so beats appearing to delete them.
func (h *handlers) patchStorage(c *gin.Context) {
	dir := strings.TrimSpace(c.PostForm("mediaDir"))
	if dir == "" {
		hxToast(c, "error", "Media directory unchanged", "A path is required.")
		renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
		return
	}

	config.GetInstance().Firmware.MediaDir = dir
	config.Save()

	hxToast(c, "success", "Settings saved", "Images already uploaded stay where they are.")
	renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
}

// postReboot answers before rebooting so the toast reaches the browser; the
// reboot then takes the connection down with it.
func (h *handlers) postReboot(c *gin.Context) {
	hxToast(c, "info", "Rebooting", "The BMC will be unreachable for about a minute.")
	c.Status(http.StatusOK)

	// Background context, NOT the gin context: *gin.Context is the request's
	// context, so CommandContext(c, …) kills `reboot` the moment this handler
	// returns — which is immediately, the response having already flushed.
	// Passing c would also outlive the request, which gin forbids without
	// c.Copy().
	go func() {
		if err := exec.CommandContext(context.Background(), "reboot").Run(); err != nil {
			h.log.Error("ui: reboot failed", slog.Any("err", err))
		}
	}()
}
