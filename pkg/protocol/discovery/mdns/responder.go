// Package mdns publishes this device's hostname and DNS-SD services over
// multicast DNS.
//
// It is a responder and nothing else: it answers questions about names it
// owns, and never browses, resolves, or caches what other hosts say. That
// asymmetry is the whole design. A general-purpose DNS-SD library has to
// unpack and cache every packet on the segment because it also implements the
// client side; measured on a single-core BMC sharing a link with a few hundred
// multicast packets per second, that cost alone saturated the processor and
// left the socket's receive queue permanently full.
//
// Here, a packet that is not a query is rejected on two bytes of its header
// (isQuery), and a query about a name we do not own is rejected by one map
// lookup (recordSet.owned). Every record is built once at Start and the
// interface is resolved once, so the receive path never touches the kernel.
//
// What is deliberately not implemented is ongoing conflict defence; see
// probe.go.
package mdns

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// generation bundles what one Start call produces, identified by the pointer
// itself: comparing r.gen against a snapshot is what lets a Stop, or a serve
// goroutine that a newer Start has superseded, tell which generation it is
// looking at.
type generation struct {
	cancel context.CancelFunc
	// done is closed by this generation's serve goroutine once its goodbyes
	// have gone out and its sockets are closed, so Stop can wait for the
	// group membership to actually be gone before the next Start joins it.
	done chan struct{}
}

// phase is the responder's state, swapped atomically so the read loops can
// observe a transition without taking a lock per packet.
type phase struct {
	rs *recordSet
	// probing marks the startup window in which foreign responses are
	// inspected for name conflicts. It is false for the rest of the
	// responder's life, which is what keeps the steady-state cost at zero.
	probing bool
}

// Responder publishes a host name and a set of services on one interface.
type Responder struct {
	host  string
	iface string
	svcs  []Service

	mu      sync.Mutex
	running bool
	claimed string
	gen     *generation
}

// New builds a responder. host is the ".local" name to claim (e.g.
// "nanokvm.local"); iface is the interface name, or "" to pick one. It does
// not touch the network until Start.
func New(host, iface string, svcs []Service) *Responder {
	return &Responder{host: host, iface: iface, svcs: svcs}
}

// Start announces and begins serving. Safe to call on an already-started
// responder: it restarts.
func (r *Responder) Start(_ context.Context) error {
	// Restart semantics: tear the previous generation down first rather than
	// layering responders. Stop blocks until its sockets are closed, so the
	// old generation has left the multicast group before this one joins —
	// otherwise the kernel delivers every packet to both, which doubles the
	// work and shows up only as unexplained CPU.
	r.Stop()

	ifi, err := resolveInterface(r.iface)
	if err != nil {
		return err
	}
	c, err := listen(ifi)
	if err != nil {
		return err
	}

	// Not derived from the caller's context: this generation's lifetime
	// belongs to Stop, which has to be able to send goodbyes after the
	// context that started it is already done.
	runCtx, cancel := context.WithCancel(context.Background())
	g := &generation{cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.gen = g
	r.running = true
	r.claimed = r.host
	r.mu.Unlock()

	go r.serve(runCtx, g, c, ifi)
	return nil
}

// Stop sends goodbyes and closes the sockets, and does not return until they
// have actually gone out. Safe to call more than once.
func (r *Responder) Stop() {
	r.mu.Lock()
	g := r.gen
	r.gen = nil
	r.mu.Unlock()

	if g == nil {
		return
	}
	g.cancel()
	<-g.done

	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// Name returns the name actually being advertised, which is not necessarily
// the one requested: probing renames on collision. Reports false when the
// responder is not running.
func (r *Responder) Name() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return "", false
	}
	return r.claimed, true
}

// serve owns one generation: probe, announce, answer, say goodbye.
func (r *Responder) serve(ctx context.Context, g *generation, c *conn, ifi *net.Interface) {
	defer close(g.done)

	var state atomic.Pointer[phase]
	conflicts := make(chan struct{}, 4)
	sends := make(chan outbound, 32)

	label := strings.TrimSuffix(r.host, "."+strings.TrimSuffix(localDomain, "."))
	ips := interfaceIPs(ifi.Name)

	// Publish an empty probing phase before the loops start, so a packet
	// arriving immediately has somewhere well-defined to land.
	state.Store(&phase{probing: true})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); r.readLoop(c, false, &state, conflicts, sends) }()
	go func() { defer wg.Done(); r.readLoop(c, true, &state, conflicts, sends) }()
	go func() { defer wg.Done(); sender(ctx, c, sends) }()

	claimed, rs, err := probe(ctx, c, label, r.svcs, ips, conflicts,
		func(ctx context.Context, round int) bool {
			// RFC 6762 §8.1: a random 0-250ms before the first probe so that
			// devices powered on together do not probe in lockstep, then
			// 250ms between rounds.
			d := 250 * time.Millisecond
			if round == 0 {
				d = time.Duration(rand.IntN(250)) * time.Millisecond
			}
			return sleepCtx(ctx, d)
		},
		func(candidate *recordSet) { state.Store(&phase{rs: candidate, probing: true}) },
	)
	if err != nil {
		c.close()
		wg.Wait()
		return
	}

	state.Store(&phase{rs: rs})
	r.mu.Lock()
	if r.gen == g {
		r.claimed = claimed + "." + strings.TrimSuffix(localDomain, ".")
	}
	r.mu.Unlock()

	// RFC 6762 §8.3: announce at least twice, a second apart, so a listener
	// that missed the first still learns the name.
	for i := range 2 {
		if i > 0 && !sleepCtx(ctx, time.Second) {
			break
		}
		broadcast(c, rs.announce)
	}

	<-ctx.Done()

	// RFC 6762 §10.1: withdraw with the same records at TTL 0, before the
	// sockets close, so browsers drop us immediately instead of waiting out
	// the TTL.
	goodbye(c, rs)
	c.close()
	wg.Wait()
}

// outbound is a response waiting to go out, with the delay it owes.
type outbound struct {
	buf   []byte
	v6    bool
	dst   *net.UDPAddr
	delay time.Duration
}

// sender applies the RFC 6762 §6.3 shared-record delay away from the read
// loops.
//
// Sleeping in a read loop is what made the previous implementation stall: one
// delayed answer held up every packet behind it, and the receive queue never
// drained. Here the read loops never block, and a flood that outruns this
// goroutine is dropped by the buffered channel rather than queued without
// bound — mDNS is best-effort, and a responder too busy to answer promptly
// should stay quiet rather than answer late.
func sender(ctx context.Context, c *conn, ch <-chan outbound) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-ch:
			if o.delay > 0 && !sleepCtx(ctx, o.delay) {
				return
			}
			_ = c.send(o.buf, o.v6, o.dst)
		}
	}
}

// readLoop is the receive path for one address family. It returns when the
// socket closes, which is how a generation ends.
func (r *Responder) readLoop(c *conn, v6 bool, state *atomic.Pointer[phase],
	conflicts chan<- struct{}, sends chan<- outbound,
) {
	buf := make([]byte, readBufSize)
	for {
		var (
			pkt packet
			err error
		)
		if v6 {
			pkt, err = c.readV6(buf)
		} else {
			pkt, err = c.readV4(buf)
		}
		if err != nil {
			return
		}

		ph := state.Load()
		if ph == nil {
			continue
		}

		if !isQuery(pkt.buf) {
			// Not a question. Worth a second look only while probing, and
			// only because a name collision has to be found before we claim
			// anything. This is the one place the responder parses another
			// host's traffic, and it lasts under a second.
			if ph.probing && ph.rs != nil {
				var msg dns.Msg
				if msg.Unpack(pkt.buf) == nil && conflictsWith(&msg, ph.rs.unique) {
					select {
					case conflicts <- struct{}{}:
					default:
					}
				}
			}
			continue
		}
		if ph.probing || ph.rs == nil {
			continue // not claiming anything yet, so nothing to answer with
		}

		var msg dns.Msg
		if msg.Unpack(pkt.buf) != nil {
			continue
		}
		resp, shared, unicast := ph.rs.respond(&msg)
		if resp == nil {
			continue
		}
		out, err := resp.Pack()
		if err != nil {
			continue
		}
		o := outbound{buf: out, v6: pkt.v6}
		if unicast {
			o.dst = pkt.from
		}
		if shared {
			o.delay = time.Duration(20+rand.IntN(100)) * time.Millisecond
		}
		select {
		case sends <- o:
		default: // too busy to answer politely; stay quiet
		}
	}
}

// broadcast sends an unsolicited response carrying rrs to both families.
func broadcast(c *conn, rrs []dns.RR) {
	if len(rrs) == 0 {
		return
	}
	m := new(dns.Msg)
	m.Response = true
	m.Authoritative = true
	m.Answer = rrs
	buf, err := m.Pack()
	if err != nil {
		return
	}
	_ = c.send(buf, false, nil)
	_ = c.send(buf, true, nil)
}

// goodbye re-sends every record with TTL 0.
func goodbye(c *conn, rs *recordSet) {
	broadcast(c, goodbyeRecords(rs))
}

// goodbyeRecords copies every announced record with its TTL zeroed.
//
// Copies, not mutations: rs.announce is still referenced by the live answer
// groups, and zeroing in place would leave a responder that answers every
// subsequent query with already-expired records.
func goodbyeRecords(rs *recordSet) []dns.RR {
	dead := make([]dns.RR, 0, len(rs.announce))
	for _, rr := range rs.announce {
		cp := dns.Copy(rr)
		cp.Header().Ttl = 0
		dead = append(dead, cp)
	}
	return dead
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// resolveInterface picks the interface to serve on.
//
// An explicitly configured name must exist. Quietly serving a different
// interface than the operator asked for would be worse than failing, because
// the symptom — discovery works, but only on the wrong network — is invisible
// from the device itself.
func resolveInterface(name string) (*net.Interface, error) {
	if name != "" {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			return nil, fmt.Errorf("mdns: interface %q: %w", name, err)
		}
		return ifi, nil
	}
	ifis, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("mdns: enumerate interfaces: %w", err)
	}
	for i := range ifis {
		ifi := &ifis[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if addrs, err := ifi.Addrs(); err == nil && len(addrs) > 0 {
			return ifi, nil
		}
	}
	return nil, fmt.Errorf("mdns: no multicast-capable interface is up")
}

// interfaceIPs resolves name's addresses once, at start.
//
// The receive path must never do this: net.InterfaceByName and
// (*net.Interface).Addrs each issue a netlink dump and allocate in proportion
// to the host's link count. Resolving once is safe because the discovery
// watcher restarts this responder whenever the interface's address list
// changes.
func interfaceIPs(name string) []net.IP {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil {
			ips = append(ips, ip)
		}
	}
	return ips
}
