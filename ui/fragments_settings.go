package ui

// fragments_settings.go serves the settings dialog. Each panel has a GET that
// renders it from live state and one or more writes that apply a change and
// answer with that same panel re-rendered — so what the user sees after a
// write is always what was persisted, never what was submitted.

import (
	"context"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	apivm "github.com/pi-bmc/nanokvm-app/api/vm"
	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/autoupdate"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/mdns"
	"github.com/pi-bmc/nanokvm-app/pkg/network"
	sshsvc "github.com/pi-bmc/nanokvm-app/pkg/ssh"
	"github.com/pi-bmc/nanokvm-app/pkg/sysinfo"
	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

func settingsFragmentRoutes(g *gin.RouterGroup) {
	s := g.Group("/settings")

	s.GET("/general", func(c *gin.Context) {
		renderFragment(c, components.SettingsGeneralBody(generalModel()))
	})
	s.PATCH("/autoupdate", patchAutoUpdate)

	s.GET("/network", func(c *gin.Context) {
		renderFragment(c, components.SettingsNetworkBody(networkModel()))
	})
	s.PATCH("/network", patchNetwork)
	s.GET("/network/status", func(c *gin.Context) {
		renderFragment(c, components.SettingsNetworkStatusRows(networkStatus()))
	})

	s.GET("/hardware", func(c *gin.Context) {
		renderFragment(c, components.SettingsHardwareBody(hardwareModel()))
	})
	s.POST("/hardware", postHardware)

	s.GET("/access", func(c *gin.Context) {
		renderFragment(c, components.SettingsAccessBody(accessModel()))
	})
	s.POST("/access/ssh", postSSHEnabled)
	s.POST("/access/ssh/keys", postSSHKeys)
	s.POST("/access/tls", postTLS)

	s.GET("/advanced", func(c *gin.Context) {
		renderFragment(c, components.SettingsAdvancedBody(advancedModel()))
	})
	s.POST("/reboot", postReboot)
}

// checked reads a checkbox out of a submitted form. An unchecked box is
// omitted by the browser entirely, which is the HTML way of saying false —
// safe here because each form always posts every switch it owns.
func checked(c *gin.Context, name string) bool {
	return c.PostForm(name) != ""
}

// ── General ─────────────────────────────────────────────────────────────

func generalModel() components.SettingsGeneral {
	au := config.GetInstance().AutoUpdate
	return components.SettingsGeneral{
		AppVersion:            application.CurrentVersion(),
		ImageVersion:          sysinfo.ImageVersion(),
		Hardware:              config.GetInstance().Hardware.Version.String(),
		AutoUpdateEnabled:     au.Enabled,
		AutoUpdateInterval:    strconv.Itoa(au.IntervalMinutes),
		AutoUpdateApplication: au.Application,
		AutoUpdateBIOS:        au.BIOS,
	}
}

func patchAutoUpdate(c *gin.Context) {
	au := &config.GetInstance().AutoUpdate
	au.Enabled = checked(c, "enabled")
	au.Application = checked(c, "application")
	au.BIOS = checked(c, "bios")
	if n, err := strconv.Atoi(c.PostForm("intervalMinutes")); err == nil && n > 0 {
		au.IntervalMinutes = n
	}

	config.Save()
	autoupdate.Start() // re-reads config; cancels an existing ticker if running

	hxToast(c, "success", "Settings saved", "Automatic update settings applied.")
	renderFragment(c, components.SettingsGeneralBody(generalModel()))
}

// ── Network ─────────────────────────────────────────────────────────────

func networkModel() components.SettingsNetwork {
	n := config.GetInstance().Network
	return components.SettingsNetwork{
		Enabled:    n.Enabled,
		Mode:       strings.ToLower(n.Eth0.Mode),
		MAC:        n.Eth0.MAC,
		Address:    n.Eth0.Address,
		Gateway:    n.Eth0.Gateway,
		DNS:        strings.Join(n.Eth0.DNS, ", "),
		RHIAddress: n.RHI.Address,
		RHILease:   n.RHI.Lease,
		Status:     networkStatus(),
	}
}

// networkStatus reports what the interfaces actually came up with, which is
// not necessarily what the form above asked for — a DHCP lease, a kernel MAC,
// or a link that failed to come up all show here.
func networkStatus() components.SettingsNetworkStatus {
	cfg := config.GetInstance().Network
	st := components.SettingsNetworkStatus{
		Mode: strings.ToUpper(strings.ToLower(cfg.Eth0.Mode)),
		MAC:  cfg.Eth0.MAC,
	}
	if st.MAC == "" {
		st.MAC = "kernel default"
	}

	for _, ip := range sysinfo.IPs() {
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
	if name, ok := mdns.Advertised(); ok {
		st.MDNS = name
	}
	return st
}

// patchNetwork validates against a copy so a rejected value leaves the live
// config untouched, then restarts the manager to apply the new addressing
// without a process restart — the same contract as PATCH /api/network/settings.
func patchNetwork(c *gin.Context) {
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
		renderFragment(c, components.SettingsNetworkBody(networkModel()))
		return
	}

	conf.Network = next
	config.Save()
	network.Restart()

	log.Infof("network: settings updated via ui (eth0 mode=%s enabled=%t)", next.Eth0.Mode, next.Enabled)
	hxToast(c, "success", "Network settings saved", "Addressing re-applied.")
	renderFragment(c, components.SettingsNetworkBody(networkModel()))
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

func hardwareModel() components.SettingsHardware {
	st := usbgadget.Get().State()
	m := components.SettingsHardware{
		USBNetwork: st.Ethernet != usbgadget.EthernetOff,
		USBDisk:    st.Disk,
		MediaState: "Not inserted",
	}
	if firmware.GetController().GetVirtualMediaState().Inserted {
		m.MediaState = "Inserted"
	}
	return m
}

// postHardware sets the gadget functions to the submitted state rather than
// toggling them: the form carries the state the user wants, and the gadget
// package no-ops when a value already matches.
func postHardware(c *gin.Context) {
	gadget := usbgadget.Get()

	mode := usbgadget.EthernetOff
	if checked(c, "network") {
		mode = usbgadget.EthernetECM
	}
	if err := gadget.SetEthernet(mode); err != nil {
		log.Errorf("ui: set ethernet %s failed: %s", mode, err)
		hxToast(c, "error", "USB Ethernet unchanged", err.Error())
	}
	if err := gadget.SetDisk(checked(c, "disk")); err != nil {
		log.Errorf("ui: set disk failed: %s", err)
		hxToast(c, "error", "USB Mass Storage unchanged", err.Error())
	}

	renderFragment(c, components.SettingsHardwareBody(hardwareModel()))
}

// ── Access ──────────────────────────────────────────────────────────────

func accessModel() components.SettingsAccess {
	keys, err := sshsvc.ReadAuthorizedKeys()
	if err != nil {
		log.Warnf("ui: read authorized keys: %v", err)
	}
	return components.SettingsAccess{
		SSHEnabled: config.GetInstance().SSH.Enabled,
		SSHKeys:    keys,
		TLSEnabled: config.GetInstance().Proto == "https",
	}
}

// postSSHEnabled starts or stops the in-process listener. A failure (the port
// is taken, say) rolls the config back, and because the response is the panel
// re-read from config the switch snaps back on its own.
func postSSHEnabled(c *gin.Context) {
	conf := config.GetInstance()
	previous := conf.SSH.Enabled
	conf.SSH.Enabled = checked(c, "enabled")

	if err := sshsvc.Restart(); err != nil {
		conf.SSH.Enabled = previous
		log.Errorf("ui: apply SSH state: %s", err)
		hxToast(c, "error", "SSH unchanged", err.Error())
	} else if previous != conf.SSH.Enabled {
		config.Save()
	}

	renderFragment(c, components.SettingsAccessBody(accessModel()))
}

func postSSHKeys(c *gin.Context) {
	keys := c.PostForm("sshKey")
	if err := sshsvc.ValidateSSHKey(keys); err != nil {
		hxToast(c, "error", "Keys not saved", err.Error())
		renderFragment(c, components.SettingsAccessBody(accessModel()))
		return
	}
	if err := sshsvc.WriteAuthorizedKeys(keys); err != nil {
		log.Errorf("ui: write authorized keys: %s", err)
		hxToast(c, "error", "Keys not saved", err.Error())
		renderFragment(c, components.SettingsAccessBody(accessModel()))
		return
	}

	hxToast(c, "success", "Authorized keys saved", "")
	renderFragment(c, components.SettingsAccessBody(accessModel()))
}

// postTLS rewrites the persisted proto and restarts the service, which is the
// only way the new listener takes effect. The response tells htmx to reload
// the page on the new scheme once the service is back.
func postTLS(c *gin.Context) {
	enable := checked(c, "enabled")

	var err error
	if enable {
		err = apivm.EnableTLS()
	} else {
		err = apivm.DisableTLS()
	}
	if err != nil {
		log.Errorf("ui: set TLS: %s", err)
		hxToast(c, "error", "TLS unchanged", err.Error())
		renderFragment(c, components.SettingsAccessBody(accessModel()))
		return
	}

	scheme := "http"
	if enable {
		scheme = "https"
	}
	hxToast(c, "info", "Restarting service", "Reconnect over "+scheme+".")
	renderFragment(c, components.SettingsAccessBody(accessModel()))

	// Exit after the response has flushed and let init respawn us.
	//
	// DELIBERATELY DETACHED: this outlives the request. If RestartService ever
	// takes a context.Context, give it context.Background() — never c or
	// c.Request.Context(). The request completes the moment this handler
	// returns, so a request-scoped context would cancel the restart before it
	// happened, and the UI would report a scheme change that never occurred.
	// Same hazard as postReboot below.
	go application.RestartService()
}

// ── Advanced ────────────────────────────────────────────────────────────

func advancedModel() components.SettingsAdvanced {
	return components.SettingsAdvanced{DeviceKey: sysinfo.DeviceKey()}
}

// postReboot answers before rebooting so the toast reaches the browser; the
// reboot then takes the connection down with it.
func postReboot(c *gin.Context) {
	hxToast(c, "info", "Rebooting", "The BMC will be unreachable for about a minute.")
	c.Status(http.StatusOK)

	// Background context, NOT the gin context: *gin.Context is the request's
	// context, so CommandContext(c, …) kills `reboot` the moment this handler
	// returns — which is immediately, the response having already flushed.
	// Passing c would also outlive the request, which gin forbids without
	// c.Copy().
	go func() {
		if err := exec.CommandContext(context.Background(), "reboot").Run(); err != nil {
			log.Errorf("ui: reboot failed: %s", err)
		}
	}()
}
