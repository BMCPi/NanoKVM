// responder.go publishes the service list built by Services onto the network
// using brutella/dnssd. It is the only file in this package that touches a
// socket — everything else stays pure so it can be unit tested without one.
package mdns

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/brutella/dnssd"
)

// generation bundles what one Start call produces, identified by the
// pointer itself: comparing r.gen against a snapshot (rather than clearing
// or reading fields unconditionally) is what lets a Stop or a superseded
// Respond goroutine tell whether it is still looking at the generation it
// started with, or whether a newer Start has since replaced it (see Start
// and Stop).
type generation struct {
	cancel context.CancelFunc
	// done is closed by this generation's Respond goroutine right before it
	// returns, so Stop/Start can block until any goodbyes that goroutine
	// sends have actually gone out before reporting stopped or starting the
	// next generation.
	done chan struct{}
}

// Responder publishes a host name and a set of services on one interface.
type Responder struct {
	host  string
	iface string
	svcs  []Service

	mu      sync.Mutex
	running bool
	gen     *generation
}

// New builds a responder. host is the ".local" name to claim (e.g.
// "nanokvm.local"); iface is the interface name, or "" for all. It does not
// touch the network until Start.
func New(host, iface string, svcs []Service) *Responder {
	return &Responder{host: host, iface: iface, svcs: svcs}
}

// Start announces and begins serving. Safe to call on an already-started
// responder: it restarts.
func (r *Responder) Start(ctx context.Context) error {
	// Restart semantics: tear down whatever the previous Start left running
	// before standing up a new one, rather than layering responders. Stop
	// blocks until the previous generation's Respond goroutine has actually
	// exited, so its goodbyes for the old instance names land on the wire
	// before this generation's announcements do — otherwise a browser sees
	// announce-then-goodbye and drops the BMC (see FINDING 3).
	r.Stop()

	resp, err := dnssd.NewResponder()
	if err != nil {
		return fmt.Errorf("mdns: new responder: %w", err)
	}

	var ifaces []string
	if r.iface != "" {
		ifaces = []string{r.iface}
	}

	// dnssd.Service.Hostname() renders as "<Host>.<Domain>.", and Domain
	// defaults to "local" when empty. Passing the full ".local" name through
	// as Host would double it up, so only the leading label goes in.
	label := strings.TrimSuffix(r.host, ".local")

	for _, svc := range r.svcs {
		s, err := dnssd.NewService(dnssd.Config{
			Name:   label,
			Type:   svc.Type,
			Host:   label,
			Text:   svc.Text,
			Port:   svc.Port,
			Ifaces: ifaces,
		})
		if err != nil {
			return fmt.Errorf("mdns: configure %s: %w", svc.Type, err)
		}
		if _, err := resp.Add(s); err != nil {
			return fmt.Errorf("mdns: add %s: %w", svc.Type, err)
		}
	}

	// Derived from the caller's ctx so an outer cancellation (or deadline)
	// stops the responder too, not just an explicit Stop().
	respondCtx, cancel := context.WithCancel(ctx)
	g := &generation{cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.gen = g
	r.running = true
	r.mu.Unlock()

	// Respond blocks for the life of the responder; it sends goodbyes and
	// closes its socket on its own once respondCtx is done, so Stop only
	// needs to cancel and then wait on g.done.
	go func() {
		defer close(g.done)
		_ = resp.Respond(respondCtx)
		r.mu.Lock()
		// Only clear running if r.gen still names this generation: a
		// goroutine from a generation Start has already superseded (see
		// the leading r.Stop() above) would otherwise still reach this
		// line eventually — dnssd's unannounce sleeps 250ms between
		// goodbye packets per interface — and stomp the running=true a
		// newer generation already set, so Name() (the package's only
		// health signal) would incorrectly report down.
		if r.gen == g {
			r.running = false
		}
		r.mu.Unlock()
	}()

	return nil
}

// Stop sends goodbyes and closes the socket, and does not return until they
// have actually gone out. Safe to call more than once.
func (r *Responder) Stop() {
	r.mu.Lock()
	g := r.gen
	r.mu.Unlock()

	if g == nil {
		return
	}
	// context.CancelFunc is inherently idempotent, and reading a channel
	// that is already closed (or racing another Stop's close of the same
	// channel) is safe too, so a concurrent Stop reaching this same
	// generation causes no harm.
	g.cancel()
	<-g.done

	// Only clear state that still belongs to this generation. A concurrent
	// Start can have already installed a newer generation by the time this
	// call wakes up from <-g.done (its own leading Stop() may have raced
	// this one to the same g above) — clobbering running/gen here would
	// then report a live responder as stopped, the same class of bug the
	// generation check in Start's goroutine guards against.
	r.mu.Lock()
	if r.gen == g {
		r.running = false
		r.gen = nil
	}
	r.mu.Unlock()
}

// Name returns the advertised host name, and whether the responder is running.
func (r *Responder) Name() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return "", false
	}
	return r.host, true
}
