package mdns

// answer.go is the receive path: decide as cheaply as possible whether a
// datagram deserves any work, and build the response when it does.
//
// Almost every multicast DNS packet on a busy segment is somebody else's
// announcement or somebody else's query. A general-purpose DNS-SD library must
// parse and cache all of it because it also implements browsing; a responder
// need not. isQuery rejects announcements from the 12-byte header without
// unpacking, and an unowned question name is rejected by one map lookup.
// Neither path allocates.

import (
	"strings"

	"github.com/miekg/dns"
)

// qmUnicast is the top bit of a question's class: "send the answer straight
// back to me rather than to the group" (RFC 6762 §5.4).
const qmUnicast = 1 << 15

// isQuery reports whether buf is worth unpacking, reading only the DNS header
// (RFC 1035 §4.1.1).
//
// A responder answers questions and nothing else, so a packet with the QR bit
// set — a response, which on a busy link is the bulk of the traffic — is of no
// interest, and neither is a query carrying no questions. Rejecting both here
// makes the common case two byte comparisons and no allocation.
func isQuery(buf []byte) bool {
	if len(buf) < 12 {
		return false
	}
	if buf[2]&0x80 != 0 {
		return false // QR set: a response
	}
	return buf[4] != 0 || buf[5] != 0 // QDCOUNT > 0
}

// respond builds the reply to a query, or nil when nothing in it is ours.
//
// shared reports that an answer is a shared record (PTR), which the caller
// must delay by a random 20-120ms before sending (RFC 6762 §6.3). unicast
// reports that the querier asked for a direct reply.
func (rs *recordSet) respond(msg *dns.Msg) (resp *dns.Msg, shared, unicast bool) {
	var answer, extra []dns.RR
	seen := make(map[string]bool)

	for _, q := range msg.Question {
		grp, ok := rs.owned[strings.ToLower(q.Name)]
		if !ok {
			continue // not a name we own
		}
		if q.Qclass&qmUnicast != 0 {
			unicast = true
		}

		for _, rr := range grp.answer {
			if !matchesType(q.Qtype, rr) {
				// Still useful context, just not an answer to what was asked.
				extra = appendUnseen(extra, seen, rr)
				continue
			}
			if knownAnswer(msg, rr) {
				continue // RFC 6762 §7.1: the querier already has it
			}
			before := len(answer)
			answer = appendUnseen(answer, seen, rr)
			if len(answer) > before && grp.shared {
				shared = true
			}
		}
		for _, rr := range grp.extra {
			extra = appendUnseen(extra, seen, rr)
		}
	}

	if len(answer) == 0 {
		// Extras alone are not an answer; sending them would be noise.
		return nil, false, false
	}

	resp = new(dns.Msg)
	resp.Response = true
	resp.Authoritative = true
	resp.Answer = answer
	resp.Extra = extra
	if unicast {
		// A unicast reply is a conventional DNS response and echoes the
		// question and id; a multicast one must not (RFC 6762 §6, §18.14).
		resp.Id = msg.Id
		resp.Question = msg.Question
	}
	return resp, shared, unicast
}

// matchesType reports whether rr answers a question of type qtype.
func matchesType(qtype uint16, rr dns.RR) bool {
	return qtype == dns.TypeANY || qtype == rr.Header().Rrtype
}

// appendUnseen adds rr unless it is already present, so a query with several
// questions cannot repeat the same record.
func appendUnseen(dst []dns.RR, seen map[string]bool, rr dns.RR) []dns.RR {
	key := rr.String()
	if seen[key] {
		return dst
	}
	seen[key] = true
	return append(dst, rr)
}

// knownAnswer reports whether the querier already listed rr in its own Answer
// section with at least half its lifetime left (RFC 6762 §7.1). Repeating it
// is pure noise on a link that is already too chatty.
func knownAnswer(msg *dns.Msg, rr dns.RR) bool {
	h := rr.Header()
	for _, known := range msg.Answer {
		kh := known.Header()
		if kh.Rrtype != h.Rrtype || !strings.EqualFold(kh.Name, h.Name) {
			continue
		}
		if kh.Ttl < h.Ttl/2 {
			continue // nearly expired: refresh it rather than suppress
		}
		// Compare the rdata, not the header, so a PTR naming a different
		// instance is not mistaken for ours.
		if rdata(known) == rdata(rr) {
			return true
		}
	}
	return false
}

// rdata renders just the record-specific part of rr, with the header stripped.
func rdata(rr dns.RR) string {
	// Presentation format is: name TTL class type rdata...
	if fields := strings.SplitN(rr.String(), "\t", 5); len(fields) == 5 {
		return fields[4]
	}
	return rr.String()
}
