package config

import (
	"bytes"
	"testing"

	"github.com/spf13/viper"
)

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

// loadConfigFromYAML drives the same viper.Unmarshal + checkDefaultValue
// sequence config.go's initialize() uses, instead of calling migrateDiscovery
// directly. A direct call can't see the absent-section backfill that runs
// after it in checkDefaultValue — which is exactly what undid the migration
// in the regression these cases cover (a "discovery.mdns key absent" check
// that didn't also account for a legacy mdns: block having just been folded
// in). jwt.secretKey is set in every fixture so checkDefaultValue doesn't
// take its needsPersist path and try to write /etc/kvm/server.yaml.
func loadConfigFromYAML(t *testing.T, yamlText string) Config {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.SetConfigType("yaml")
	if err := viper.ReadConfig(bytes.NewBufferString(yamlText)); err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}

	instance = Config{}
	if err := viper.Unmarshal(&instance); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	checkDefaultValue()
	return instance
}

// TestDiscoveryMigrationSurvivesDefaulting is the regression test for commit
// 7f46907: the discovery.mdns absent-section backfill ran unconditionally on
// !viper.IsSet("discovery.mdns"), which is also true for every legacy-only
// file (it has no discovery: block at all), so it stomped a just-migrated
// legacy mdns: block back to hardcoded defaults — silently losing a
// non-default interface/hostname and, worse, reviving a deliberately
// disabled responder. Exercises the real load path (viper +
// checkDefaultValue) so it can actually see that backfill run, unlike a test
// that calls migrateDiscovery in isolation.
func TestDiscoveryMigrationSurvivesDefaulting(t *testing.T) {
	const secret = "jwt:\n  secretKey: test-secret\n"

	for _, tc := range []struct {
		name          string
		yaml          string
		wantInterface string
		wantHostname  string
		wantEnabled   bool
	}{
		{
			name:          "legacy-only",
			yaml:          secret + "mdns:\n  enabled: true\n  interface: eth1\n  hostname: old\n",
			wantInterface: "eth1",
			wantHostname:  "old",
			wantEnabled:   true,
		},
		{
			name:          "discovery-only",
			yaml:          secret + "discovery:\n  mdns:\n    enabled: true\n    interface: eth2\n    hostname: new\n",
			wantInterface: "eth2",
			wantHostname:  "new",
			wantEnabled:   true,
		},
		{
			// Both spellings present: the explicit discovery: block wins in
			// full, per TestExplicitDiscoveryBlockWinsOverLegacy — the legacy
			// eth1 must not leak through even partially.
			name:          "both",
			yaml:          secret + "mdns:\n  enabled: true\n  interface: eth1\n  hostname: old\n" + "discovery:\n  mdns:\n    interface: eth0\n",
			wantInterface: "eth0",
			wantHostname:  "",
			wantEnabled:   false,
		},
		{
			name:          "neither",
			yaml:          secret,
			wantInterface: "eth0",
			wantHostname:  "",
			wantEnabled:   true,
		},
		{
			// CRITICAL 2 from the review: a deliberately disabled legacy
			// responder must stay disabled after migration, not get flipped
			// back on by the eth0/true/true/true defaults.
			name:          "legacy-disabled",
			yaml:          secret + "mdns:\n  enabled: false\n  interface: eth1\n",
			wantInterface: "eth1",
			wantHostname:  "",
			wantEnabled:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := loadConfigFromYAML(t, tc.yaml)

			if c.Discovery.MDNS.Interface != tc.wantInterface {
				t.Errorf("Discovery.MDNS.Interface = %q, want %q", c.Discovery.MDNS.Interface, tc.wantInterface)
			}
			if c.Discovery.MDNS.Hostname != tc.wantHostname {
				t.Errorf("Discovery.MDNS.Hostname = %q, want %q", c.Discovery.MDNS.Hostname, tc.wantHostname)
			}
			if c.Discovery.MDNS.Enabled != tc.wantEnabled {
				t.Errorf("Discovery.MDNS.Enabled = %v, want %v", c.Discovery.MDNS.Enabled, tc.wantEnabled)
			}
		})
	}
}
