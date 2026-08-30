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

// Responder publishes a host name and a set of services on one interface.
type Responder struct {
	host  string
	iface string
	svcs  []Service

	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	stopOnce *sync.Once
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
	// before standing up a new one, rather than layering responders.
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

	r.mu.Lock()
	r.cancel = cancel
	r.stopOnce = &sync.Once{}
	r.running = true
	r.mu.Unlock()

	// Respond blocks for the life of the responder; it sends goodbyes and
	// closes its socket on its own once respondCtx is done, so Stop only
	// needs to cancel.
	go func() {
		_ = resp.Respond(respondCtx)
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	return nil
}

// Stop sends goodbyes and closes the socket. Safe to call more than once.
func (r *Responder) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	once := r.stopOnce
	r.mu.Unlock()

	if cancel == nil {
		return
	}
	// once, not a nil check on r.cancel, guards the actual cancel: Stop can
	// race Start from the background watcher, and reading then calling
	// cancel without it would let two callers both see a non-nil func and
	// both "stop" — harmless for context.CancelFunc itself, but the pattern
	// matches the mutex-guarded sibling responder in pkg/mdns and keeps this
	// safe if the guarded body ever grows beyond a single idempotent call.
	once.Do(cancel)

	r.mu.Lock()
	r.running = false
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
