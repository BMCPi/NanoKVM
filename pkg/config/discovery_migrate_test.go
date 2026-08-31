package config

// discovery_migrate_test.go covers the *persistence* half of the legacy
// mdns: -> discovery.mdns migration; discovery_test.go covers the in-memory
// fold and the defaulting that runs after it.
//
// The two halves are separate tests because folding in memory only was not
// harmless. Config.MDNS was marshalled back out on every Save(), so an
// upgraded file carried both spellings — and a file that has a discovery:
// key makes migrateDiscovery skip the legacy block entirely on the next
// load. The operator's legacy values were therefore read exactly once and
// then silently discarded. Forcing a one-time rewrite that drops the legacy
// key leaves a single source of truth on disk instead.

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// jwt.secretKey is set in every fixture so the JWT branch of
// checkDefaultValue never takes the needsPersist path — a rewrite in these
// tests then means the migration asked for it, and nothing else.
const migrateSecret = "jwt:\n  secretKey: test-secret\n"

// readRewritten returns the rewritten file's top-level keys and its raw
// bytes, failing the test if no rewrite happened.
func readRewritten(t *testing.T, path string) (map[string]any, []byte) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config was never rewritten: %v", err)
	}

	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		t.Fatalf("rewritten config is not valid YAML: %v\n%s", err, data)
	}
	return top, data
}

// TestLegacyMDNSBlockIsDroppedFromDisk is the point of the whole change: a
// board that boots with only the legacy spelling must come back up with only
// the new one.
func TestLegacyMDNSBlockIsDroppedFromDisk(t *testing.T) {
	c, path := loadConfigFromYAML(t, migrateSecret+
		"mdns:\n  enabled: true\n  interface: eth0\n  hostname: bmc-a\n")

	if c.Discovery.MDNS.Interface != "eth0" || c.Discovery.MDNS.Hostname != "bmc-a" ||
		!c.Discovery.MDNS.Enabled {
		t.Fatalf("legacy values did not migrate: %+v", c.Discovery.MDNS)
	}

	top, data := readRewritten(t, path)
	if _, ok := top["mdns"]; ok {
		t.Errorf("rewritten config still carries a legacy mdns: key:\n%s", data)
	}
	if _, ok := top["discovery"]; !ok {
		t.Errorf("rewritten config has no discovery: key:\n%s", data)
	}
}

// TestMigratedConfigRoundTrips catches a marshal that writes something the
// loader reads back differently — the failure mode a "no mdns: key" check
// alone would miss. It also pins the "once" in one-time rewrite: an
// already-migrated file has nothing left to persist.
func TestMigratedConfigRoundTrips(t *testing.T) {
	first, path := loadConfigFromYAML(t, migrateSecret+
		"mdns:\n  enabled: true\n  interface: eth0\n  hostname: bmc-a\n")
	_, data := readRewritten(t, path)

	second, secondPath := loadConfigFromYAML(t, string(data))

	if second.Discovery.MDNS != first.Discovery.MDNS {
		t.Errorf("reloading the rewritten config changed Discovery.MDNS:\nfirst  %+v\nsecond %+v\nfile:\n%s",
			first.Discovery.MDNS, second.Discovery.MDNS, data)
	}
	if _, err := os.Stat(secondPath); err == nil {
		t.Errorf("an already-migrated config was rewritten a second time")
	}
}

// TestExplicitDiscoveryWinsAndLegacyKeyIsDropped guards the invariant that
// forcing the migration must not turn a stale legacy block into a way to
// overwrite an explicit new one. The legacy block is deleted, never read.
func TestExplicitDiscoveryWinsAndLegacyKeyIsDropped(t *testing.T) {
	c, path := loadConfigFromYAML(t, migrateSecret+
		"mdns:\n  enabled: true\n  interface: eth1\n  hostname: old\n"+
		"discovery:\n  mdns:\n    interface: eth0\n")

	if c.Discovery.MDNS.Interface != "eth0" || c.Discovery.MDNS.Hostname != "" {
		t.Errorf("a stale legacy block leaked into an explicit discovery: block: %+v",
			c.Discovery.MDNS)
	}

	top, data := readRewritten(t, path)
	if _, ok := top["mdns"]; ok {
		t.Errorf("stale legacy mdns: key survived the rewrite:\n%s", data)
	}
}

// TestLegacyDisabledSurvivesMigrationAndReload is the persistence-side
// version of the legacy-disabled case in discovery_test.go: an operator who
// turned the responder off must not have it revived by the rewrite, nor by
// the load of the file the rewrite produced.
func TestLegacyDisabledSurvivesMigrationAndReload(t *testing.T) {
	first, path := loadConfigFromYAML(t, migrateSecret+
		"mdns:\n  enabled: false\n  interface: eth1\n")

	if first.Discovery.MDNS.Enabled {
		t.Fatalf("a deliberately disabled legacy responder was revived: %+v", first.Discovery.MDNS)
	}

	_, data := readRewritten(t, path)
	second, _ := loadConfigFromYAML(t, string(data))

	if second.Discovery.MDNS.Enabled {
		t.Errorf("the disabled responder came back on reloading the rewritten config: %+v\n%s",
			second.Discovery.MDNS, data)
	}
	if second.Discovery.MDNS.Interface != "eth1" {
		t.Errorf("Discovery.MDNS.Interface = %q, want eth1", second.Discovery.MDNS.Interface)
	}
}

// TestConfigWithoutEitherBlockIsNotRewritten keeps the migration from
// touching files that have nothing to migrate — every boot of an
// already-current device would otherwise rewrite /etc/kvm/server.yaml.
func TestConfigWithoutEitherBlockIsNotRewritten(t *testing.T) {
	c, path := loadConfigFromYAML(t, migrateSecret)

	want := MDNS{Enabled: true, Interface: "eth0", IPv4: true, IPv6: true}
	if c.Discovery.MDNS != want {
		t.Errorf("Discovery.MDNS = %+v, want the defaults %+v", c.Discovery.MDNS, want)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("a config with nothing to migrate was rewritten anyway")
	}
}
