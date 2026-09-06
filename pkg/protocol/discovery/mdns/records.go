package mdns

// records.go turns the service list into every DNS record this responder will
// ever send, once, at Start.
//
// Answering a query is then a map lookup and a slice copy. Nothing here is
// recomputed per packet, and nothing consults the kernel: the interface and
// its addresses are resolved once by the caller. See the package doc comment
// on responder.go for why that matters.

import (
	"net"
	"slices"
	"strings"

	"github.com/miekg/dns"
)

const (
	// localDomain is the only domain multicast DNS serves (RFC 6762 §3).
	localDomain = "local."

	// metaQueryName is the DNS-SD service type enumeration name (RFC 6763
	// §9). Answering it is what makes this device appear to a browser that
	// is discovering service types rather than a specific one.
	metaQueryName = "_services._dns-sd._udp." + localDomain

	// hostTTL is the TTL for records naming the host itself — A, AAAA and
	// SRV. RFC 6762 §10 wants these short, so a host that moves or leaves is
	// forgotten quickly.
	hostTTL = 120

	// sharedTTL is the TTL for PTR and TXT, which describe a service rather
	// than a host's current address (RFC 6762 §10).
	sharedTTL = 4500

	// cacheFlush is the top bit of the class field: "this is the complete,
	// authoritative answer for this name and type, replace anything cached"
	// (RFC 6762 §10.2). Set on unique records only — never on PTR, which is
	// shared with every other responder offering that service type.
	cacheFlush = 1 << 15
)

// answerGroup is the prepared response for one owned name.
type answerGroup struct {
	// answer and extra are the two response sections. They are shared by
	// every response built from them, so they are read-only after Start.
	answer []dns.RR
	extra  []dns.RR
	// shared marks a name whose records are shared between responders (PTR).
	// RFC 6762 §6.3 says to answer those after a random 20-120ms delay, so
	// that simultaneous responders do not collide.
	shared bool
}

// recordSet is everything the responder can answer, indexed by question name.
type recordSet struct {
	// owned maps a lowercased, fully-qualified question name to its prepared
	// answer. A question whose name is absent is not ours and is dropped
	// with no further work: this single map lookup is the entire cost of
	// rejecting another host's query.
	owned map[string]*answerGroup

	// announce is every record, for the unsolicited announcements at start
	// and the TTL-0 goodbyes at stop (RFC 6762 §8.3, §10.1).
	announce []dns.RR

	// unique is the subset whose names this responder claims exclusively,
	// which is what probing has to test for (RFC 6762 §8.1).
	unique []dns.RR
}

// escapeLabel escapes the characters DNS presentation format reserves, so an
// instance label containing a dot cannot silently split into two labels.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `.`, `\.`).Replace(s)
}

// buildRecords prepares every answer for label (the instance and host name,
// without a domain), the services, and the addresses to advertise.
func buildRecords(label string, svcs []Service, ips []net.IP) *recordSet {
	hostname := dns.Fqdn(label + "." + localDomain)
	rs := &recordSet{owned: make(map[string]*answerGroup, len(svcs)*2+2)}

	addrs := addressRecords(hostname, ips)

	// The host's own name answers with its addresses and nothing else.
	if len(addrs) > 0 {
		rs.owned[strings.ToLower(hostname)] = &answerGroup{answer: addrs}
		rs.unique = append(rs.unique, addrs...)
		rs.announce = append(rs.announce, addrs...)
	}

	var metaPTRs []dns.RR
	for _, svc := range svcs {
		typeName := dns.Fqdn(svc.Type + "." + localDomain)
		instance := dns.Fqdn(escapeLabel(label) + "." + svc.Type + "." + localDomain)

		srv := &dns.SRV{
			Hdr:    dns.RR_Header{Name: instance, Rrtype: dns.TypeSRV, Class: dns.ClassINET | cacheFlush, Ttl: hostTTL},
			Target: hostname,
			Port:   uint16(svc.Port), //nolint:gosec // G115: Service.Port is a TCP port, 0..65535 by construction
		}
		txt := &dns.TXT{
			Hdr: dns.RR_Header{Name: instance, Rrtype: dns.TypeTXT, Class: dns.ClassINET | cacheFlush, Ttl: sharedTTL},
			Txt: txtStrings(svc.Text),
		}
		ptr := &dns.PTR{
			// No cache-flush: PTR is shared, and claiming exclusivity would
			// tell browsers to discard every other host offering this type.
			Hdr: dns.RR_Header{Name: typeName, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: sharedTTL},
			Ptr: instance,
		}

		// A browse for the type: PTR answers it, the rest saves a round trip.
		rs.owned[strings.ToLower(typeName)] = &answerGroup{
			answer: []dns.RR{ptr},
			extra:  append([]dns.RR{srv, txt}, addrs...),
			shared: true,
		}
		// A direct lookup of the instance: SRV and TXT are the answer.
		rs.owned[strings.ToLower(instance)] = &answerGroup{
			answer: []dns.RR{srv, txt},
			extra:  addrs,
		}

		metaPTRs = append(metaPTRs, &dns.PTR{
			Hdr: dns.RR_Header{Name: metaQueryName, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: sharedTTL},
			Ptr: typeName,
		})
		rs.unique = append(rs.unique, srv, txt)
		rs.announce = append(rs.announce, ptr, srv, txt)
	}

	if len(metaPTRs) > 0 {
		rs.owned[metaQueryName] = &answerGroup{answer: metaPTRs, shared: true}
	}
	return rs
}

// addressRecords renders the advertised addresses as A/AAAA for hostname.
//
// Link-local IPv6 is skipped: it is only reachable on the same link and
// carries a zone that does not survive being written into a DNS record, so
// publishing it hands clients an address they cannot use.
func addressRecords(hostname string, ips []net.IP) []dns.RR {
	var out []dns.RR
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, &dns.A{
				Hdr: dns.RR_Header{Name: hostname, Rrtype: dns.TypeA, Class: dns.ClassINET | cacheFlush, Ttl: hostTTL},
				A:   v4,
			})
			continue
		}
		if ip.To16() == nil || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, &dns.AAAA{
			Hdr:  dns.RR_Header{Name: hostname, Rrtype: dns.TypeAAAA, Class: dns.ClassINET | cacheFlush, Ttl: hostTTL},
			AAAA: ip.To16(),
		})
	}
	return out
}

// txtStrings renders a TXT map as key=value strings.
//
// A DNS-SD TXT record is never empty: zero-length rdata is not a legal TXT, so
// RFC 6763 §6.1 says a service with no attributes carries a single empty
// string instead. Keys are sorted so the record is byte-stable across
// restarts, which is what keeps the cache-flush bit honest — an unstable
// record would look like a change on every announcement.
func txtStrings(text map[string]string) []string {
	if len(text) == 0 {
		return []string{""}
	}
	keys := make([]string, 0, len(text))
	for k := range text {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+text[k])
	}
	return out
}
