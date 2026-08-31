// responder.go serves the UPnP/SSDP multicast group: it joins
// 239.255.255.250:1900, answers M-SEARCH with the messages message.go builds,
// and sends periodic ssdp:alive / a final ssdp:byebye. It is the only file in
// this package that touches a socket — message.go stays pure so it can be
// unit tested without one.
package ssdp

import (
	"context"
	"math/rand"
	"net"
	"sync"
	"time"
)

var ssdpGroup = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

// Config is what the responder needs; no pkg/config import.
type Config struct {
	Iface    string // interface name; "" = all
	UUID     string
	Location string // service root URL, e.g. "https://10.0.0.5/redfish/v1/"
	Minor    int    // Redfish protocol minor, from redfishProtocolVersion
	MaxAge   int    // seconds
}

// generation bundles everything one Start call produces, so a Stop can only
// ever tear down the generation it snapshotted rather than reaching into
// whatever r.gen has been replaced with by the time it gets the lock back
// (see Stop).
type generation struct {
	conn     *net.UDPConn
	cancel   context.CancelFunc
	stopOnce sync.Once
}

// Responder answers M-SEARCH requests and announces ssdp:alive / ssdp:byebye
// on the UPnP multicast group.
type Responder struct {
	cfg Config

	mu  sync.Mutex
	gen *generation
}

// New builds a responder. It does not touch the network until Start.
func New(cfg Config) *Responder {
	return &Responder{cfg: cfg}
}

// Start joins the group, sends ssdp:alive, and serves M-SEARCH until ctx ends.
func (r *Responder) Start(ctx context.Context) error {
	// Restart semantics: tear down whatever a previous Start left running.
	r.Stop()

	ifi, err := resolveInterface(r.cfg.Iface)
	if err != nil {
		return err
	}

	conn, err := net.ListenMulticastUDP("udp4", ifi, ssdpGroup)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)

	g := &generation{conn: conn, cancel: cancel}

	r.mu.Lock()
	r.gen = g
	r.mu.Unlock()

	go r.serve(runCtx, conn)
	go r.announce(runCtx, conn)

	return nil
}

// resolveInterface maps an interface name to *net.Interface, or nil (all
// interfaces) for "". An empty name is what makes the point-to-point usb0
// host link safe from BMC discovery traffic — that scoping is set by the
// caller, not defaulted here.
func resolveInterface(name string) (*net.Interface, error) {
	if name == "" {
		return nil, nil
	}
	return net.InterfaceByName(name)
}

// serve reads datagrams until runCtx is done. A matching M-SEARCH answers
// after a random delay in its own goroutine, per RFC/UPnP MX semantics — a
// blocking sleep here would stall the read loop and drop every search that
// arrives during the wait.
func (r *Responder) serve(runCtx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		select {
		case <-runCtx.Done():
			return
		default:
		}

		// A short deadline lets the loop notice runCtx.Done() promptly
		// instead of blocking forever in ReadFromUDP after Stop.
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if runCtx.Err() != nil {
				return
			}
			continue
		}

		raw := make([]byte, n)
		copy(raw, buf[:n])

		search, ok := ParseSearch(raw, r.cfg.Minor)
		if !ok {
			continue
		}

		go r.reply(runCtx, conn, from, search)
	}
}

// reply waits the search's MX-bounded random delay, then unicasts our
// answer. Run per search in its own goroutine — see serve's comment.
func (r *Responder) reply(runCtx context.Context, conn *net.UDPConn, from *net.UDPAddr, search Search) {
	if search.MX > 0 {
		// Draw uniformly across the whole [0, MX) interval, not across whole
		// seconds: rand.Int63n(int64(search.MX)) picks one of only MX
		// integer-second values, so MX=1 (the default for a missing/
		// unparseable MX header) always drew exactly 0s — no delay at all,
		// which is the fleet-wide simultaneous-response pile-up the delay
		// exists to spread out.
		delay := time.Duration(rand.Int63n(int64(time.Duration(search.MX) * time.Second)))
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-runCtx.Done():
			return
		}
	}

	resp := Response(r.cfg.UUID, r.cfg.Location, r.cfg.Minor, r.cfg.MaxAge)
	_, _ = conn.WriteToUDP(resp, from)
}

// announce sends ssdp:alive immediately and then every MaxAge/2 seconds, the
// UPnP-recommended re-announce interval so caches never expire between
// announcements.
func (r *Responder) announce(runCtx context.Context, conn *net.UDPConn) {
	r.sendNotify(conn, true)

	interval := time.Duration(r.cfg.MaxAge/2) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
			r.sendNotify(conn, true)
		}
	}
}

func (r *Responder) sendNotify(conn *net.UDPConn, alive bool) {
	msg := Notify(r.cfg.UUID, r.cfg.Location, r.cfg.Minor, r.cfg.MaxAge, alive)
	_, _ = conn.WriteToUDP(msg, ssdpGroup)
}

// Stop sends ssdp:byebye and closes. Safe to call more than once.
func (r *Responder) Stop() {
	r.mu.Lock()
	g := r.gen
	r.mu.Unlock()

	if g == nil {
		return
	}
	g.stopOnce.Do(func() {
		// Byebye must go out before the socket closes, or the goodbye
		// never leaves — closing first would just drop the write.
		r.sendNotify(g.conn, false)
		g.cancel()
		_ = g.conn.Close()
	})

	// Only clear r.gen if it still names the generation we just tore down.
	// A concurrent Start can have already installed a newer generation
	// while this call was blocked on the lock or on stopOnce above (e.g.
	// Start's own leading Stop() ran first, tore down gen N, and started
	// gen N+1) — clearing r.gen here would then null out the live gen N+1
	// with nothing left holding a reference to it, leaking its socket
	// forever since no future Stop() could ever reach it again.
	r.mu.Lock()
	if r.gen == g {
		r.gen = nil
	}
	r.mu.Unlock()
}
