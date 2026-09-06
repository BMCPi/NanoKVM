package mdns

import (
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func testRecords(t *testing.T) *recordSet {
	t.Helper()
	return buildRecords("nanokvm", []Service{
		{Type: "_redfish._tcp", Port: 443, Text: map[string]string{"path": "/redfish/v1/", "txtvers": "1"}},
		{Type: "_http._tcp", Port: 80},
	}, []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::1"), net.ParseIP("fe80::1")})
}

// Every name a browser can legitimately ask us about must be owned, because an
// unowned name is dropped with no further work.
func TestOwnedNamesCoverTheAdvertisedSurface(t *testing.T) {
	rs := testRecords(t)
	for _, name := range []string{
		"nanokvm.local.",
		"_redfish._tcp.local.",
		"nanokvm._redfish._tcp.local.",
		"_http._tcp.local.",
		"nanokvm._http._tcp.local.",
		"_services._dns-sd._udp.local.",
	} {
		if _, ok := rs.owned[strings.ToLower(name)]; !ok {
			t.Errorf("%s is not owned — a query for it would be dropped", name)
		}
	}
	if _, ok := rs.owned["_ipp._tcp.local."]; ok {
		t.Error("_ipp._tcp.local. is owned but was never advertised")
	}
}

// The cache-flush bit asserts "this is the whole truth for this name". It
// belongs on records we alone own, and must never appear on PTR, which is
// shared with every other host offering the same service type.
func TestCacheFlushOnUniqueRecordsOnly(t *testing.T) {
	rs := testRecords(t)
	for _, rr := range rs.announce {
		flush := rr.Header().Class&cacheFlush != 0
		_, isPTR := rr.(*dns.PTR)
		if isPTR && flush {
			t.Errorf("PTR %s carries cache-flush; it would evict other hosts' records", rr.Header().Name)
		}
		if !isPTR && !flush {
			t.Errorf("%T %s is missing the cache-flush bit", rr, rr.Header().Name)
		}
	}
}

// Link-local IPv6 carries a zone that cannot survive a DNS record, so
// publishing it hands clients an address they cannot reach.
func TestLinkLocalIPv6IsNotPublished(t *testing.T) {
	rs := testRecords(t)
	grp := rs.owned["nanokvm.local."]
	if grp == nil {
		t.Fatal("hostname not owned")
	}
	var v4, v6 int
	for _, rr := range grp.answer {
		switch a := rr.(type) {
		case *dns.A:
			v4++
		case *dns.AAAA:
			v6++
			if a.AAAA.IsLinkLocalUnicast() {
				t.Errorf("published link-local %s", a.AAAA)
			}
		}
	}
	if v4 != 1 || v6 != 1 {
		t.Errorf("got %d A and %d AAAA, want 1 and 1", v4, v6)
	}
}

// RFC 6763 §6.1: a service with no attributes gets one empty string, never a
// zero-length rdata, which is not a legal TXT record.
func TestTXTIsNeverEmpty(t *testing.T) {
	rs := buildRecords("n", []Service{{Type: "_http._tcp", Port: 80}}, []net.IP{net.ParseIP("192.0.2.1")})
	grp := rs.owned["n._http._tcp.local."]
	if grp == nil {
		t.Fatal("instance not owned")
	}
	for _, rr := range grp.answer {
		if txt, ok := rr.(*dns.TXT); ok {
			if len(txt.Txt) != 1 || txt.Txt[0] != "" {
				t.Errorf("TXT = %q, want exactly one empty string", txt.Txt)
			}
			return
		}
	}
	t.Error("no TXT record was built")
}

// TXT contents must be byte-stable across restarts, or every restart looks
// like a change to a cache holding the previous copy.
func TestTXTOrderIsStable(t *testing.T) {
	text := map[string]string{"z": "1", "a": "2", "m": "3"}
	var first string
	for i := range 5 {
		rs := buildRecords("n", []Service{{Type: "_x._tcp", Port: 1, Text: text}}, nil)
		txt, ok := rs.owned["n._x._tcp.local."].answer[1].(*dns.TXT)
		if !ok {
			t.Fatal("expected a TXT as the second answer record")
		}
		got := strings.Join(txt.Txt, ",")
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("TXT order changed between builds: %q then %q", first, got)
		}
	}
	if first != "a=2,m=3,z=1" {
		t.Errorf("TXT = %q, want sorted", first)
	}
}

// A dot in the instance label must not silently split the name in two.
func TestInstanceLabelIsEscaped(t *testing.T) {
	rs := buildRecords("my.kvm", []Service{{Type: "_http._tcp", Port: 80}}, nil)
	if _, ok := rs.owned[`my\.kvm._http._tcp.local.`]; !ok {
		keys := make([]string, 0, len(rs.owned))
		for k := range rs.owned {
			keys = append(keys, k)
		}
		t.Errorf("escaped instance name not owned; got %v", keys)
	}
}
