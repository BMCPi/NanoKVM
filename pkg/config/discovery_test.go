package config

import "testing"

// TestLegacyMDNSBlockMigratesIntoDiscovery covers upgrading a config written
// before discovery.mdns existed: the top-level mdns: block must still land
// somewhere the SSDP/mDNS responders read from.
func TestLegacyMDNSBlockMigratesIntoDiscovery(t *testing.T) {
	c := &Config{MDNS: MDNS{Enabled: true, Interface: "eth1", Hostname: "old"}}
	migrateDiscovery(c, false /* discoveryKeySet */)

	if c.Discovery.MDNS.Interface != "eth1" || c.Discovery.MDNS.Hostname != "old" {
		t.Errorf("legacy mdns: block did not migrate: %+v", c.Discovery.MDNS)
	}
}

// TestExplicitDiscoveryBlockWinsOverLegacy covers a config that has both
// spellings (e.g. a legacy mdns: block left behind after hand-editing in a
// discovery: block) — the explicit discovery: block must never be clobbered
// by the legacy one.
func TestExplicitDiscoveryBlockWinsOverLegacy(t *testing.T) {
	c := &Config{
		MDNS:      MDNS{Interface: "eth1"},
		Discovery: Discovery{MDNS: MDNS{Interface: "eth0"}},
	}
	migrateDiscovery(c, true /* discoveryKeySet */)

	if c.Discovery.MDNS.Interface != "eth0" {
		t.Errorf("legacy block overwrote an explicit discovery block: %q",
			c.Discovery.MDNS.Interface)
	}
}
