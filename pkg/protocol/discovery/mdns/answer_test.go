package mdns

import (
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func pack(t *testing.T, m *dns.Msg) []byte {
	t.Helper()
	buf, err := m.Pack()
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	return buf
}

// A foreign announcement — the bulk of the traffic on a busy segment — must be
// rejected from the header, before anything is unpacked or allocated. This is
// the property the responder is built around: without it we are back to paying
// per-packet costs for other hosts' chatter.
func TestForeignResponseIsRejectedFromTheHeader(t *testing.T) {
	other := new(dns.Msg)
	other.Response = true
	other.Answer = []dns.RR{&dns.PTR{
		Hdr: dns.RR_Header{Name: "_printer._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 120},
		Ptr: "someone-else._printer._tcp.local.",
	}}
	buf := pack(t, other)

	if isQuery(buf) {
		t.Fatal("a response was accepted as a query")
	}
	if n := testing.AllocsPerRun(200, func() { _ = isQuery(buf) }); n != 0 {
		t.Errorf("rejecting a foreign response allocated %.0f times, want 0", n)
	}
}

func TestHeaderRejectionEdgeCases(t *testing.T) {
	q := new(dns.Msg)
	q.SetQuestion("_http._tcp.local.", dns.TypePTR)
	good := pack(t, q)

	for _, tc := range []struct {
		name string
		buf  []byte
		want bool
	}{
		{"real query", good, true},
		{"no questions", pack(t, new(dns.Msg)), false},
		{"truncated header", good[:11], false},
		{"empty", nil, false},
	} {
		if got := isQuery(tc.buf); got != tc.want {
			t.Errorf("%s: isQuery = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A query for a name we do not own must produce nothing at all.
func TestUnownedQuestionIsNotAnswered(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion("_printer._tcp.local.", dns.TypePTR)

	if resp, _, _ := rs.respond(q); resp != nil {
		t.Errorf("answered a question about someone else's service: %v", resp.Answer)
	}
}

// A browse for a type we advertise: PTR is the answer, SRV/TXT/A ride along so
// the browser needs no follow-up, and the shared-record delay is owed.
func TestBrowseForOurTypeIsAnswered(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion("_redfish._tcp.local.", dns.TypePTR)

	resp, shared, unicast := rs.respond(q)
	if resp == nil {
		t.Fatal("no response to a browse for our own service type")
	}
	if !shared {
		t.Error("PTR is a shared record; the RFC 6762 §6.3 delay should be owed")
	}
	if unicast {
		t.Error("no QU bit was set, so the reply should be multicast")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("Answer = %v, want exactly the PTR", resp.Answer)
	}
	ptr, ok := resp.Answer[0].(*dns.PTR)
	if !ok || ptr.Ptr != "nanokvm._redfish._tcp.local." {
		t.Errorf("Answer[0] = %v, want a PTR to our instance", resp.Answer[0])
	}
	var srv, txt, a bool
	for _, rr := range resp.Extra {
		switch rr.(type) {
		case *dns.SRV:
			srv = true
		case *dns.TXT:
			txt = true
		case *dns.A:
			a = true
		}
	}
	if !srv || !txt || !a {
		t.Errorf("Extra missing records (srv=%v txt=%v a=%v): %v", srv, txt, a, resp.Extra)
	}
	if !resp.Response || !resp.Authoritative {
		t.Error("response must be marked Response+Authoritative")
	}
	if resp.Question != nil {
		t.Error("a multicast response must not echo the question (RFC 6762 §6)")
	}
}

// A question's type filters what lands in Answer: asking for A must not be
// answered with AAAA, though the AAAA is still useful as additional data.
func TestQuestionTypeFiltersTheAnswer(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion("nanokvm.local.", dns.TypeA)

	resp, _, _ := rs.respond(q)
	if resp == nil {
		t.Fatal("no response to a hostname query")
	}
	for _, rr := range resp.Answer {
		if _, ok := rr.(*dns.A); !ok {
			t.Errorf("Answer contains %T for an A question", rr)
		}
	}
	if len(resp.Answer) != 1 {
		t.Errorf("Answer = %v, want one A record", resp.Answer)
	}
}

// RFC 6762 §7.1: a querier that already holds a healthy copy should not be
// told again. On a link this chatty, suppression is not an optimisation.
func TestKnownAnswerSuppression(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion("_redfish._tcp.local.", dns.TypePTR)
	q.Answer = []dns.RR{&dns.PTR{
		Hdr: dns.RR_Header{Name: "_redfish._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: sharedTTL},
		Ptr: "nanokvm._redfish._tcp.local.",
	}}

	if resp, _, _ := rs.respond(q); resp != nil {
		t.Errorf("re-sent a record the querier already listed: %v", resp.Answer)
	}
}

// A nearly-expired known answer must be refreshed rather than suppressed.
func TestNearlyExpiredKnownAnswerIsRefreshed(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion("_redfish._tcp.local.", dns.TypePTR)
	q.Answer = []dns.RR{&dns.PTR{
		Hdr: dns.RR_Header{Name: "_redfish._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 1},
		Ptr: "nanokvm._redfish._tcp.local.",
	}}

	if resp, _, _ := rs.respond(q); resp == nil {
		t.Error("suppressed an answer whose cached copy was about to expire")
	}
}

// The QU bit asks for a direct reply, which is a conventional DNS response and
// therefore echoes the question and the transaction id.
func TestUnicastQuestionIsAnsweredDirectly(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.Id = 0x1234
	q.Question = []dns.Question{{
		Name: "_http._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET | qmUnicast,
	}}

	resp, _, unicast := rs.respond(q)
	if resp == nil {
		t.Fatal("no response")
	}
	if !unicast {
		t.Error("QU bit was set but the reply was not marked unicast")
	}
	if resp.Id != 0x1234 || len(resp.Question) != 1 {
		t.Errorf("unicast reply must echo id and question; got id=%#x q=%v", resp.Id, resp.Question)
	}
}

// A responder with no addresses still answers service browses; it simply has
// no A/AAAA to offer.
func TestNoAddressesStillAnswersBrowse(t *testing.T) {
	rs := buildRecords("n", []Service{{Type: "_http._tcp", Port: 80}}, nil)
	if _, ok := rs.owned["n.local."]; ok {
		t.Error("hostname owned despite having no addresses to answer with")
	}
	q := new(dns.Msg)
	q.SetQuestion("_http._tcp.local.", dns.TypePTR)
	if resp, _, _ := rs.respond(q); resp == nil {
		t.Error("no response to a browse when the host has no addresses")
	}
}

// The meta-query is how a browser discovers which service types exist here.
func TestMetaQueryListsEveryType(t *testing.T) {
	rs := testRecords(t)
	q := new(dns.Msg)
	q.SetQuestion(metaQueryName, dns.TypePTR)

	resp, _, _ := rs.respond(q)
	if resp == nil {
		t.Fatal("no response to the service-type enumeration")
	}
	got := map[string]bool{}
	for _, rr := range resp.Answer {
		if ptr, ok := rr.(*dns.PTR); ok {
			got[ptr.Ptr] = true
		}
	}
	for _, want := range []string{"_redfish._tcp.local.", "_http._tcp.local."} {
		if !got[want] {
			t.Errorf("meta-query did not list %s (got %v)", want, got)
		}
	}
}

var sinkResp *dns.Msg

// The cost that matters most after header rejection: a fully unpacked query
// for somebody else's name.
func BenchmarkRespondToForeignQuery(b *testing.B) {
	rs := buildRecords("nanokvm", []Service{{Type: "_http._tcp", Port: 80}}, []net.IP{net.ParseIP("192.0.2.10")})
	q := new(dns.Msg)
	q.SetQuestion("_printer._tcp.local.", dns.TypePTR)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkResp, _, _ = rs.respond(q)
	}
}

// A goodbye is the same records at TTL 0 (RFC 6762 §10.1). It must not mutate
// the live set: a responder that zeroed its own records in place would answer
// every later query with something already expired.
func TestGoodbyeZeroesTTLWithoutMutatingTheLiveRecords(t *testing.T) {
	rs := testRecords(t)
	if len(rs.announce) == 0 {
		t.Fatal("nothing to announce")
	}

	dead := goodbyeRecords(rs)
	if len(dead) != len(rs.announce) {
		t.Fatalf("goodbye covers %d records, announce has %d", len(dead), len(rs.announce))
	}
	for _, rr := range dead {
		if rr.Header().Ttl != 0 {
			t.Errorf("goodbye record %s has TTL %d, want 0", rr.Header().Name, rr.Header().Ttl)
		}
	}
	for _, rr := range rs.announce {
		if rr.Header().Ttl == 0 {
			t.Errorf("live record %s was zeroed in place by goodbye", rr.Header().Name)
		}
	}
}

// Everything we can answer with must also be announced, or a browser that
// misses the query window never learns about it.
func TestAnnounceCoversEveryOwnedName(t *testing.T) {
	rs := testRecords(t)
	announced := map[string]bool{}
	for _, rr := range rs.announce {
		announced[strings.ToLower(rr.Header().Name)] = true
	}
	for name := range rs.owned {
		if name == metaQueryName {
			continue // answered on demand; not announced unsolicited
		}
		if !announced[name] {
			t.Errorf("%s is answerable but never announced", name)
		}
	}
}
