package config

// usbgadget_test.go covers usbGadget.serialConsole defaulting. Unlike the
// other gadget toggles it defaults OFF: composing the acm function costs the
// one free IN endpoint the SG2002's dwc2 core has left, so an existing
// /etc/kvm/server.yaml that predates the key must come up exactly as it did
// before — the same composite, the same endpoint budget.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSerialConsoleAbsentKeyDefaultsFalse is the upgrade case: every config
// written before this feature omits the key, and none of those boards may
// grow a USB serial function (and lose their last IN endpoint) on upgrade.
func TestSerialConsoleAbsentKeyDefaultsFalse(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret)

	if c.UsbGadget.SerialConsole {
		t.Error("UsbGadget.SerialConsole = true for a config with no serialConsole key, want false")
	}
}

// TestSerialConsoleExplicitTruePreserved: the operator opting in must survive
// defaulting. (The zero value and the default agree here, so the only way to
// get this wrong is to overwrite the field unconditionally.)
func TestSerialConsoleExplicitTruePreserved(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret+"usbGadget:\n  serialConsole: true\n")

	if !c.UsbGadget.SerialConsole {
		t.Error("UsbGadget.SerialConsole = false, want the operator's explicit true")
	}
}

// TestSerialConsoleDoesNotDisturbOtherGadgetToggles guards the rest of the
// stanza: adding a key to usbGadget must not change how the neighbouring
// default-true toggles are resolved.
func TestSerialConsoleDoesNotDisturbOtherGadgetToggles(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret+"usbGadget:\n  serialConsole: true\n  hid: false\n")

	if c.UsbGadget.HID {
		t.Error("UsbGadget.HID = true, want the operator's explicit false")
	}
	if !c.UsbGadget.Disk {
		t.Error("UsbGadget.Disk = false, want the default true")
	}
	if c.UsbGadget.Ethernet != "eem" {
		t.Errorf("UsbGadget.Ethernet = %q, want the default \"eem\"", c.UsbGadget.Ethernet)
	}
}

// TestSerialConsoleDefaultConfigIsOff pins the compiled-in default a freshly
// created /etc/kvm/server.yaml is written from.
func TestSerialConsoleDefaultConfigIsOff(t *testing.T) {
	if defaultConfig.UsbGadget.SerialConsole {
		t.Error("defaultConfig.UsbGadget.SerialConsole = true, want false")
	}
}

// TestSerialConsoleYAMLKeySpelling pins the key name the field marshals to.
// config.Save() rewrites the whole struct, so the spelling here is the
// spelling an operator's file ends up carrying; the settings UI, this
// package's defaulting and the docs all name usbGadget.serialConsole.
func TestSerialConsoleYAMLKeySpelling(t *testing.T) {
	c, _ := loadConfigFromYAML(t, migrateSecret+"usbGadget:\n  serialConsole: true\n")

	data, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "serialConsole: true") {
		t.Errorf("marshalled config does not carry serialConsole: true:\n%s", data)
	}
}
