package ssdp

import (
	"strings"
	"testing"
)

func TestParseSearchAcceptsEveryServedTarget(t *testing.T) {
	for _, st := range []string{
		"ssdp:all",
		"upnp:rootdevice",
		"urn:dmtf-org:service:redfish-rest:1",
		"urn:dmtf-org:service:redfish-rest:1:13",
	} {
		raw := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n" +
			"MAN: \"ssdp:discover\"\r\nMX: 3\r\nST: " + st + "\r\n\r\n")
		if _, ok := ParseSearch(raw, 13); !ok {
			t.Errorf("ST %q was not accepted", st)
		}
	}
}

func TestParseSearchRejectsForeignTraffic(t *testing.T) {
	cases := map[string][]byte{
		"NOTIFY, not a search": []byte("NOTIFY * HTTP/1.1\r\nNTS: ssdp:alive\r\n\r\n"),
		"missing MAN":          []byte("M-SEARCH * HTTP/1.1\r\nST: ssdp:all\r\n\r\n"),
		"someone else's ST": []byte("M-SEARCH * HTTP/1.1\r\n" +
			"MAN: \"ssdp:discover\"\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n"),
		"garbage": []byte("\x00\x01\x02"),
	}
	for name, raw := range cases {
		if _, ok := ParseSearch(raw, 13); ok {
			t.Errorf("%s: accepted, want ignored", name)
		}
	}
}

// MX is clamped: a client asking us to wait a minute does not get to.
func TestParseSearchClampsMX(t *testing.T) {
	mk := func(mx string) []byte {
		return []byte("M-SEARCH * HTTP/1.1\r\nMAN: \"ssdp:discover\"\r\nMX: " +
			mx + "\r\nST: ssdp:all\r\n\r\n")
	}
	for in, want := range map[string]int{"3": 3, "60": 5, "0": 0, "": 1, "wat": 1} {
		s, ok := ParseSearch(mk(in), 13)
		if !ok {
			t.Fatalf("MX %q: not accepted", in)
		}
		if s.MX != want {
			t.Errorf("MX %q parsed to %d, want %d", in, s.MX, want)
		}
	}
}

// The wire format is DMTF's, verified against their reference mockup server.
func TestResponseMatchesDMTFFormat(t *testing.T) {
	got := string(Response("aaaa-bbbb", "https://10.0.0.5/redfish/v1/", 13, 1800))

	for _, want := range []string{
		"HTTP/1.1 200 OK\r\n",
		"CACHE-CONTROL: max-age=1800\r\n",
		"ST: urn:dmtf-org:service:redfish-rest:1:13\r\n",
		"USN: uuid:aaaa-bbbb::urn:dmtf-org:service:redfish-rest:1:13\r\n",
		"AL: https://10.0.0.5/redfish/v1/\r\n",
		"EXT:\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response missing %q\ngot:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\r\n\r\n") {
		t.Error("response must end with a blank line")
	}
}

// DMTF's client reads AL and nothing else; generic UPnP browsers read
// LOCATION. Send both, identical.
func TestResponseCarriesALAndLocation(t *testing.T) {
	got := string(Response("u", "https://host/redfish/v1/", 13, 1800))
	if !strings.Contains(got, "AL: https://host/redfish/v1/\r\n") ||
		!strings.Contains(got, "LOCATION: https://host/redfish/v1/\r\n") {
		t.Errorf("want AL and LOCATION with the same value, got:\n%s", got)
	}
}

func TestNotifyAliveAndByebye(t *testing.T) {
	alive := string(Notify("u", "https://host/redfish/v1/", 13, 1800, true))
	if !strings.HasPrefix(alive, "NOTIFY * HTTP/1.1\r\n") ||
		!strings.Contains(alive, "NTS: ssdp:alive\r\n") ||
		!strings.Contains(alive, "HOST: 239.255.255.250:1900\r\n") {
		t.Errorf("bad alive:\n%s", alive)
	}

	bye := string(Notify("u", "https://host/redfish/v1/", 13, 1800, false))
	if !strings.Contains(bye, "NTS: ssdp:byebye\r\n") {
		t.Errorf("bad byebye:\n%s", bye)
	}
	if strings.Contains(bye, "AL:") {
		t.Error("byebye must not advertise a location")
	}
}
