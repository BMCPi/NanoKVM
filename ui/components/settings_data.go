package components

// settings_data.go holds the view models the settings panels render. They are
// plain data: ui/fragments_settings.go gathers them from pkg/* and passes them
// in, both for the first paint and for every htmx swap, so one template
// definition covers both.

// SettingsGeneral backs the General panel — the BMC's own identity plus the
// background updater's configuration.
type SettingsGeneral struct {
	AppVersion   string
	ImageVersion string
	Hardware     string

	AutoUpdateEnabled     bool
	AutoUpdateInterval    string
	AutoUpdateApplication bool
	AutoUpdateBIOS        bool

	// ConsolePrimaryView is "serial" or "hdmi" — which view the dashboard
	// opens on. Carried as the raw sentinel so it can be handed straight to
	// the select's Value.
	ConsolePrimaryView string
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
// BMC presents to the managed host.
type SettingsHardware struct {
	USBNetwork bool
	USBDisk    bool
	MediaState string
}

// SettingsAccess backs the Access panel — SSH, TLS and the web account.
type SettingsAccess struct {
	SSHEnabled bool
	SSHKeys    string
	TLSEnabled bool
}

// SettingsAdvanced backs the Advanced panel.
type SettingsAdvanced struct {
	DeviceKey string
}
