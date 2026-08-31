package config

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

// TestLegacyMDNSBlockMigratesIntoDiscovery covers upgrading a config written
// before discovery.mdns existed: the top-level mdns: block must still land
// somewhere the SSDP/mDNS responders read from.
func TestLegacyMDNSBlockMigratesIntoDiscovery(t *testing.T) {
	c := &Config{MDNS: &MDNS{Enabled: true, Interface: "eth1", Hostname: "old"}}
	migrateDiscovery(c, false /* discoveryKeySet */)

	if c.Discovery.MDNS.Interface != "eth1" || c.Discovery.MDNS.Hostname != "old" {
		t.Errorf("legacy mdns: block did not migrate: %+v", c.Discovery.MDNS)
	}
	// Cleared so the rewrite that follows drops the key rather than
	// re-emitting it alongside discovery:.
	if c.MDNS != nil {
		t.Errorf("legacy mdns: block survived the migration: %+v", c.MDNS)
	}
}

// TestExplicitDiscoveryBlockWinsOverLegacy covers a config that has both
// spellings (e.g. a legacy mdns: block left behind after hand-editing in a
// discovery: block) — the explicit discovery: block must never be clobbered
// by the legacy one.
func TestExplicitDiscoveryBlockWinsOverLegacy(t *testing.T) {
	c := &Config{
		MDNS:      &MDNS{Interface: "eth1"},
		Discovery: Discovery{MDNS: MDNS{Interface: "eth0"}},
	}
	migrateDiscovery(c, true /* discoveryKeySet */)

	if c.Discovery.MDNS.Interface != "eth0" {
		t.Errorf("legacy block overwrote an explicit discovery block: %q",
			c.Discovery.MDNS.Interface)
	}
	// Ignored, but still dropped: leaving it would keep two spellings of the
	// same setting in the file the next Save() writes.
	if c.MDNS != nil {
		t.Errorf("stale legacy mdns: block survived the migration: %+v", c.MDNS)
	}
}

// loadConfigFromYAML drives the same viper.Unmarshal + checkDefaultValue
// sequence config.go's initialize() uses, instead of calling migrateDiscovery
// directly. A direct call can't see the absent-section backfill that runs
// after it in checkDefaultValue — which is exactly what undid the migration
// in the regression these cases cover (a "discovery.mdns key absent" check
// that didn't also account for a legacy mdns: block having just been folded
// in). jwt.secretKey is set in every fixture so checkDefaultValue doesn't
// take its needsPersist path for the generated secret.
//
// checkDefaultValue can still persist — a legacy mdns: block asks for the
// one-time migration rewrite — so the config file is redirected into
// t.TempDir() and its path returned. No test writes /etc/kvm, and a test
// that cares can assert on the rewritten bytes, or on the file's absence.
func loadConfigFromYAML(t *testing.T, yamlText string) (Config, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "server.yaml")
	origPath := configFilePath
	configFilePath = path
	t.Cleanup(func() { configFilePath = origPath })

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
	return instance, path
}

// TestDiscoveryMigrationSurvivesDefaulting is the regression test for commit
// 7f46907: the discovery.mdns absent-section backfill ran unconditionally on
// !viper.IsSet("discovery.mdns"), which is also true for every legacy-only
// file (it has no discovery: block at all), so it stomped a just-migrated
// legacy mdns: block back to hardcoded defaults — silently losing a
// non-default interface/hostname and, worse, reviving a deliberately
// disabled responder. Also covers the round-2 fix: Enabled/IPv4/IPv6 must be
// backfilled per-key (like Interface already was), or a bare
// `discovery: {mdns: {interface: eth0}}` lands with the responder disabled
// simply because it never repeats "enabled: true". Exercises the real load
// path (viper + checkDefaultValue) so it can actually see the backfill that
// runs after migrateDiscovery, unlike a test that calls migrateDiscovery in
// isolation.
func TestDiscoveryMigrationSurvivesDefaulting(t *testing.T) {
	const secret = "jwt:\n  secretKey: test-secret\n"

	for _, tc := range []struct {
		name          string
		yaml          string
		wantInterface string
		wantHostname  string
		wantEnabled   bool
		wantIPv4      bool
		wantIPv6      bool
	}{
		{
			name:          "legacy-only",
			yaml:          secret + "mdns:\n  enabled: true\n  interface: eth1\n  hostname: old\n",
			wantInterface: "eth1",
			wantHostname:  "old",
			wantEnabled:   true,
			wantIPv4:      true,
			wantIPv6:      true,
		},
		{
			name:          "discovery-only",
			yaml:          secret + "discovery:\n  mdns:\n    enabled: true\n    interface: eth2\n    hostname: new\n",
			wantInterface: "eth2",
			wantHostname:  "new",
			wantEnabled:   true,
			wantIPv4:      true,
			wantIPv6:      true,
		},
		{
			// Both spellings present: the explicit discovery: block wins in
			// full, per TestExplicitDiscoveryBlockWinsOverLegacy — the legacy
			// eth1 must not leak through even partially, and legacy's
			// enabled: true must not be what makes Enabled true here either;
			// it's true because discovery.mdns's own (absent) enabled key
			// defaults true, same as discovery-interface-only below.
			name:          "both",
			yaml:          secret + "mdns:\n  enabled: true\n  interface: eth1\n  hostname: old\n" + "discovery:\n  mdns:\n    interface: eth0\n",
			wantInterface: "eth0",
			wantHostname:  "",
			wantEnabled:   true,
			wantIPv4:      true,
			wantIPv6:      true,
		},
		{
			name:          "neither",
			yaml:          secret,
			wantInterface: "eth0",
			wantHostname:  "",
			wantEnabled:   true,
			wantIPv4:      true,
			wantIPv6:      true,
		},
		{
			// CRITICAL 2 from round 1: a deliberately disabled legacy
			// responder must stay disabled after migration, not get flipped
			// back on by the eth0/true/true/true defaults. This is also the
			// regression guard for the round-2 trap: a naive per-key
			// backfill keyed only on discovery.mdns.enabled (ignoring which
			// spelling authored the value) would see "enabled" as unset and
			// revive it.
			name:          "legacy-disabled",
			yaml:          secret + "mdns:\n  enabled: false\n  interface: eth1\n",
			wantInterface: "eth1",
			wantHostname:  "",
			wantEnabled:   false,
			wantIPv4:      true,
			wantIPv6:      true,
		},
		{
			// Round-2 regression case: an operator opting into the new
			// spelling with only an interface override, no legacy block at
			// all, and no explicit "enabled" anywhere — must still get a
			// running responder, not Go's bool zero value.
			name:          "discovery-interface-only",
			yaml:          secret + "discovery:\n  mdns:\n    interface: eth0\n",
			wantInterface: "eth0",
			wantHostname:  "",
			wantEnabled:   true,
			wantIPv4:      true,
			wantIPv6:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := loadConfigFromYAML(t, tc.yaml)

			if c.Discovery.MDNS.Interface != tc.wantInterface {
				t.Errorf("Discovery.MDNS.Interface = %q, want %q", c.Discovery.MDNS.Interface, tc.wantInterface)
			}
			if c.Discovery.MDNS.Hostname != tc.wantHostname {
				t.Errorf("Discovery.MDNS.Hostname = %q, want %q", c.Discovery.MDNS.Hostname, tc.wantHostname)
			}
			if c.Discovery.MDNS.Enabled != tc.wantEnabled {
				t.Errorf("Discovery.MDNS.Enabled = %v, want %v", c.Discovery.MDNS.Enabled, tc.wantEnabled)
			}
			if c.Discovery.MDNS.IPv4 != tc.wantIPv4 {
				t.Errorf("Discovery.MDNS.IPv4 = %v, want %v", c.Discovery.MDNS.IPv4, tc.wantIPv4)
			}
			if c.Discovery.MDNS.IPv6 != tc.wantIPv6 {
				t.Errorf("Discovery.MDNS.IPv6 = %v, want %v", c.Discovery.MDNS.IPv6, tc.wantIPv6)
			}
		})
	}
}
