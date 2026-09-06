package mdns

// probe.go implements the name conflict check RFC 6762 §8.1 requires before a
// responder may claim a name.
//
// Scope, deliberately: this responder probes at start and never again. Ongoing
// conflict *defence* (§9) would mean parsing every response on the segment in
// steady state, which is exactly the cost this package exists to avoid — see
// the package doc comment. Two devices booting with the same name is the case
// that actually happens, and probing catches it. A conflict appearing later
// goes unnoticed until the next restart, which the discovery watcher already
// triggers on any interface change.

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// maxProbeAttempts bounds renaming. RFC 6762 §8.1 wants a responder that keeps
// colliding to back off rather than rename forever; ten distinct names is well
// past the point where something else is wrong.
const maxProbeAttempts = 10

// conflictsWith reports whether a foreign response claims one of our unique
// names with different data — the definition of a conflict in §8.1.
func conflictsWith(msg *dns.Msg, unique []dns.RR) bool {
	if !msg.Response {
		return false
	}
	for _, theirs := range append(append([]dns.RR{}, msg.Answer...), msg.Extra...) {
		th := theirs.Header()
		for _, ours := range unique {
			oh := ours.Header()
			if th.Rrtype != oh.Rrtype || !strings.EqualFold(th.Name, oh.Name) {
				continue
			}
			if rdata(theirs) != rdata(ours) {
				return true
			}
		}
	}
	return false
}

// probeQuery is the query sent while probing: ask for anything at our names,
// and put what we intend to claim in the authority section so that a
// simultaneous prober can break the tie (§8.2).
func probeQuery(rs *recordSet) *dns.Msg {
	m := new(dns.Msg)
	seen := map[string]bool{}
	for _, rr := range rs.unique {
		name := rr.Header().Name
		if seen[name] {
			continue
		}
		seen[name] = true
		m.Question = append(m.Question, dns.Question{
			Name: name, Qtype: dns.TypeANY, Qclass: dns.ClassINET,
		})
	}
	m.Ns = rs.unique
	return m
}

// incrementLabel produces the next candidate name after a conflict, following
// the "name-2, name-3" convention Bonjour and Avahi both use. An existing
// numeric suffix is replaced rather than stacked.
func incrementLabel(label string, n int) string {
	base := label
	if i := strings.LastIndex(label, "-"); i > 0 {
		if suffix := label[i+1:]; suffix != "" && strings.Trim(suffix, "0123456789") == "" {
			base = label[:i]
		}
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// probe claims a name, renaming on conflict, and returns the records for the
// name it won. Conflicts arrive on conflicts, fed by the read loops while they
// are in probing mode; publish hands each candidate's records to those loops,
// which need the unique set to recognise a conflicting response.
//
// A probe nobody answers is a success: silence means nobody else holds the
// name.
func probe(ctx context.Context, c *conn, label string, svcs []Service, ips []net.IP,
	conflicts <-chan struct{}, sleep func(context.Context, int) bool,
	publish func(*recordSet),
) (string, *recordSet, error) {
	for attempt := range maxProbeAttempts {
		candidate := label
		if attempt > 0 {
			candidate = incrementLabel(label, attempt+1)
		}
		rs := buildRecords(candidate, svcs, ips)
		publish(rs)
		if len(rs.unique) == 0 {
			return candidate, rs, nil // nothing exclusive to claim
		}

		// Discard conflicts reported against the previous candidate.
		for len(conflicts) > 0 {
			<-conflicts
		}

		if runProbeRounds(ctx, c, rs, conflicts, sleep) {
			return candidate, rs, nil
		}
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
	}
	return "", nil, fmt.Errorf("mdns: %q is still contested after %d attempts", label, maxProbeAttempts)
}

// runProbeRounds sends the three probes and reports whether the name is ours.
func runProbeRounds(ctx context.Context, c *conn, rs *recordSet,
	conflicts <-chan struct{}, sleep func(context.Context, int) bool,
) bool {
	q := probeQuery(rs)
	buf, err := q.Pack()
	if err != nil {
		return true // cannot probe for a name we cannot encode; claim it
	}
	for round := range 3 {
		if !sleep(ctx, round) {
			return false
		}
		select {
		case <-conflicts:
			return false
		default:
		}
		_ = c.send(buf, false, nil)
		_ = c.send(buf, true, nil)
	}
	// One last look after the final round.
	select {
	case <-conflicts:
		return false
	default:
		return true
	}
}
