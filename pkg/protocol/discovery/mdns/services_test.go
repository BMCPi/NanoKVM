package mdns

import "testing"

func TestServicesForDefaultHTTPSDeployment(t *testing.T) {
	got := Services(Inputs{
		Proto: "https", HTTPPort: 80, HTTPSPort: 443,
		RedfishEnabled: true, SSHEnabled: true, SSHPort: 22,
		UUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	})

	byType := map[string]Service{}
	for _, s := range got {
		byType[s.Type] = s
	}

	if s, ok := byType["_redfish._tcp"]; !ok || s.Port != 443 {
		t.Errorf("_redfish._tcp = %+v, want port 443", s)
	}
	if s, ok := byType["_https._tcp"]; !ok || s.Port != 443 {
		t.Errorf("_https._tcp = %+v, want port 443", s)
	}
	if s, ok := byType["_http._tcp"]; !ok || s.Port != 80 {
		t.Errorf("_http._tcp = %+v, want port 80", s)
	}
	if s, ok := byType["_ssh._tcp"]; !ok || s.Port != 22 {
		t.Errorf("_ssh._tcp = %+v, want port 22", s)
	}
	if _, ok := byType["_ipmi._udp"]; ok {
		t.Error("_ipmi._udp advertised; it is deliberately out of scope")
	}
}

// With proto: http there is no TLS listener, so Redfish is reachable on the
// plain port and _https._tcp must not be advertised at all.
func TestServicesForPlaintextDeployment(t *testing.T) {
	got := Services(Inputs{
		Proto: "http", HTTPPort: 8090, HTTPSPort: 8443, RedfishEnabled: true,
	})
	for _, s := range got {
		if s.Type == "_https._tcp" {
			t.Error("_https._tcp advertised while proto is http")
		}
		if s.Type == "_redfish._tcp" && s.Port != 8090 {
			t.Errorf("_redfish._tcp port = %d, want the http port 8090", s.Port)
		}
	}
}

func TestDisabledSubsystemsAreNotAdvertised(t *testing.T) {
	got := Services(Inputs{
		Proto: "https", HTTPPort: 80, HTTPSPort: 443,
		RedfishEnabled: false, SSHEnabled: false, SSHPort: 22,
	})
	for _, s := range got {
		if s.Type == "_redfish._tcp" || s.Type == "_ssh._tcp" {
			t.Errorf("%s advertised while its subsystem is disabled", s.Type)
		}
	}
}

func TestRedfishTXTCarriesIdentity(t *testing.T) {
	got := Services(Inputs{
		Proto: "https", HTTPSPort: 443, RedfishEnabled: true,
		UUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	})
	for _, s := range got {
		if s.Type != "_redfish._tcp" {
			continue
		}
		want := map[string]string{
			"txtvers":   "1",
			"protovers": "1.0",
			"uuid":      "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"path":      "/redfish/v1/",
		}
		for k, v := range want {
			if s.Text[k] != v {
				t.Errorf("TXT %s = %q, want %q", k, s.Text[k], v)
			}
		}
		return
	}
	t.Fatal("_redfish._tcp missing")
}

// An unknown UUID must be omitted rather than published as an empty value: a
// consumer keying on uuid= would otherwise record every BMC under "".
func TestRedfishTXTOmitsEmptyUUID(t *testing.T) {
	got := Services(Inputs{Proto: "https", HTTPSPort: 443, RedfishEnabled: true})
	for _, s := range got {
		if s.Type == "_redfish._tcp" {
			if _, ok := s.Text["uuid"]; ok {
				t.Error("uuid= present with no stable identity")
			}
		}
	}
}
