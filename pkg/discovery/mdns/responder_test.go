package mdns

import (
	"context"
	"testing"
	"time"

	"github.com/brutella/dnssd"
)

// TestResponderAnnouncesServicesOnLoopback is the only test of the DNS-SD
// responder, so it has to actually observe an announcement, not just the
// running flag New() would report for any Start that merely flips it: a
// Start body reduced to "r.mu.Lock(); r.running = true; r.mu.Unlock(); return
// nil" — no dnssd.NewResponder, no NewService, no Add, no Respond, zero
// records ever put on the wire — would still satisfy a Name()-only
// assertion. Browsing for the exact service this test registers is a proof
// something was actually announced that such a no-op Start cannot fake.
func TestResponderAnnouncesServicesOnLoopback(t *testing.T) {
	r := New("nanokvm-test.local", "lo", []Service{
		{Type: "_redfish._tcp", Port: 8443, Text: map[string]string{"txtvers": "1"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	name, ok := r.Name()
	if !ok || name != "nanokvm-test.local" {
		t.Fatalf("Name() = %q, %v; want the advertised host", name, ok)
	}

	// Browse for the service the responder above should have registered.
	// LookupType blocks for the life of browseCtx, so it needs its own
	// goroutine; found is buffered so a late/duplicate Add can't block it
	// once the test has already moved on.
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer browseCancel()

	// This host is on a real, live WLAN, not an isolated test network:
	// dnssd.LookupType browses every multicast-capable interface (there is
	// no way to scope it to "lo" from the public API), so real devices
	// elsewhere on the LAN can and do answer for common service types.
	// Filtering to our own instance name is what keeps this test honest
	// against that noise instead of passing on someone else's printer.
	const wantName = "nanokvm-test"

	found := make(chan dnssd.BrowseEntry, 4)
	go func() {
		_ = dnssd.LookupType(browseCtx, "_redfish._tcp.local.", func(e dnssd.BrowseEntry) {
			if e.Name != wantName {
				return
			}
			select {
			case found <- e:
			default:
			}
		}, func(dnssd.BrowseEntry) {})
	}()

	select {
	case e := <-found:
		if e.Type != "_redfish._tcp" {
			t.Errorf("browsed entry type = %q, want %q", e.Type, "_redfish._tcp")
		}
		if e.Port != 8443 {
			t.Errorf("browsed entry port = %d, want 8443", e.Port)
		}
	case <-browseCtx.Done():
		t.Fatal("never observed the advertised _redfish._tcp service over mDNS browse; a no-op Start would fail here too")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	r := New("nanokvm-test.local", "lo", nil)
	r.Stop()
	r.Stop() // must not panic
}

// TestRestartDoesNotReportDownWhileNewGenerationRuns reproduces FINDING 3's
// clobber deterministically, exactly as the finding describes: Start(ctx)
// then Start(ctx), no concurrency required. Start's leading Stop() tears
// down the first generation without waiting for its Respond goroutine, so
// that goroutine later reaches "r.running = false" (with no generation
// check) well after the second Start already set running = true — dnssd's
// unannounce sleeps 250ms between goodbye packets per interface, so the
// stale write lands hundreds of milliseconds later, and Name() (the
// package's only health signal, and what the vm-info endpoint reports)
// reads as down for the rest of the responder's life even though a second
// generation is actively serving.
func TestRestartDoesNotReportDownWhileNewGenerationRuns(t *testing.T) {
	r := New("nanokvm-test.local", "lo", []Service{
		{Type: "_redfish._tcp", Port: 8443},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	// Long enough for the first generation's Respond goroutine to have
	// reached its post-unannounce "r.running = false" line even accounting
	// for dnssd's 250ms-per-interface goodbye pacing.
	time.Sleep(500 * time.Millisecond)

	if name, ok := r.Name(); !ok {
		t.Errorf("Name() = %q, %v after a clean restart and a 500ms settle; the first generation's exit clobbered running (FINDING 3)", name, ok)
	}
}

// TestHostLabelStripsTheLocalSuffix covers the .local trimming in Start:
// dnssd.Service.Hostname() renders as "<Host>.<Domain>." and Domain
// defaults to "local", so passing the full ".local" name through as Host
// would double it up, publishing SRV/A records under
// "nanokvm-test.local.local." instead of "nanokvm-test.local.".
//
// The browse-side cache can't be asked for the raw wire name directly — its
// parseHostname only ever keeps the first two dot-separated labels off
// whatever string it's given, so a browsed entry's Host normalizes to
// "nanokvm-test" whether or not the bug is present and can't distinguish
// them. What the double-suffix bug actually breaks is address resolution:
// the responder answers A-record queries only for the exact owner name it
// registered (Hostname()), so a doubled-up "nanokvm-test.local.local."
// leaves nothing answering the correctly-formed "nanokvm-test.local."
// queries a browser sends, and the browsed entry's IPs never populate.
// Requiring a non-empty IPs here fails under that bug and passes once the
// suffix is stripped correctly.
func TestHostLabelStripsTheLocalSuffix(t *testing.T) {
	r := New("nanokvm-test.local", "lo", []Service{
		{Type: "_http._tcp", Port: 8080},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	browseCtx, browseCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer browseCancel()

	// See the same note in TestResponderAnnouncesServicesOnLoopback: this
	// host is on a real WLAN, and _http._tcp is a common real-world service
	// type, so filtering to our own instance name is required here, not
	// just good hygiene — without it a stray printer or router answering
	// for "_http._tcp" would let this test pass regardless of what Start
	// actually published.
	const wantName = "nanokvm-test"

	found := make(chan dnssd.BrowseEntry, 4)
	go func() {
		_ = dnssd.LookupType(browseCtx, "_http._tcp.local.", func(e dnssd.BrowseEntry) {
			if e.Name != wantName {
				return
			}
			select {
			case found <- e:
			default:
			}
		}, func(dnssd.BrowseEntry) {})
	}()

	deadline := time.After(8 * time.Second)
	for {
		select {
		case e := <-found:
			if len(e.IPs) > 0 {
				return
			}
			// The first Add can fire before the A record arrives; keep
			// waiting for a later Add carrying an address instead of
			// failing on this one.
		case <-deadline:
			t.Fatal("never resolved an address for the advertised host; a doubled \".local.local.\" suffix would look exactly like this")
		}
	}
}
