package discovery

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// TestLocalNames moved verbatim from the old hostname-only responder package
// this one replaced — the hostname normalization it pins did not change when
// the responders it feeds did.
func TestLocalNames(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"licheervnano", []string{"licheervnano.local"}},
		{"NanoKVM", []string{"nanokvm.local"}},
		{"host.local", []string{"host.local"}},
		{"host.local.", []string{"host.local"}},
		{"  Spaced  ", []string{"spaced.local"}},
		{"UPPER.LOCAL", []string{"upper.local"}},
		{"", nil},
		{"   ", nil},
		{".", nil},
	}
	for _, tc := range cases {
		got := localNames(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("localNames(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The watcher's restart trigger must include the SSDP location, because the
// AL header embeds the BMC's address: an address change that left SSDP alone
// would advertise a URL nothing answers on.
func TestSignatureChangesWithAddresses(t *testing.T) {
	r := &Responder{ifaceName: "", hostname: "fixed"}
	first := r.signature(nil)
	if first == "" {
		t.Fatal("signature is empty; the watcher can never detect a change")
	}
	if first != r.signature(nil) {
		t.Error("signature is unstable across calls with unchanged inputs")
	}
}

// Start must not touch a socket when both protocols are switched off:
// "no discovery configured" is a legitimate deployment choice, not an error,
// and the caller may be running with no multicast available at all (e.g. a
// test sandbox).
func TestStartIsANoopWhenBothProtocolsDisabled(t *testing.T) {
	cfg := config.GetInstance()
	origDiscovery, origRedfish := cfg.Discovery, cfg.Redfish
	t.Cleanup(func() {
		cfg.Discovery = origDiscovery
		cfg.Redfish = origRedfish
	})

	// Zero value: MDNS.Enabled and SSDP.Enabled both false. Redfish is left
	// enabled to prove SSDP's own Enabled flag is what gates it here, not a
	// side effect of Redfish being off too.
	cfg.Discovery = config.Discovery{}
	cfg.Redfish = config.Redfish{Enabled: true}

	r, err := Start(context.Background(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if r != nil {
		t.Fatal("Start() returned a non-nil Responder with both protocols disabled")
	}
}

// TestStopFencesAgainstARacingStart is the regression test for a watcher
// tick racing Stop(): the tick reads mu, releases it, and is about to call
// start() when a concurrent Restart() retires this Responder out from under
// it. Without the stopped fence, the tick's start() would reacquire mu and
// bind a fresh responder nobody owns — leaked forever, since the watcher's
// next loop selects stopCh and returns. mdnsEnabled is true here so a
// missing fence would attempt a real socket bind, not just skip a no-op.
func TestStopFencesAgainstARacingStart(t *testing.T) {
	r := &Responder{
		mdnsEnabled: true,
		hostname:    "fixed",
		stopCh:      make(chan struct{}),
	}

	r.Stop()

	// Simulate the racing watcher tick's start() call landing after Stop()
	// has already retired r.
	if err := r.start(); err != nil {
		t.Fatalf("start() after Stop() returned an error: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mdnsR != nil || r.ssdpR != nil {
		t.Fatal("start() bound a responder on an already-stopped Responder")
	}
}

// TestSSDPHostPortOmitsOnlyTheSchemeDefault is the regression test for the
// SSDP Location/AL URL silently dropping a non-default port (finding: an
// https deployment on the shipped 8443 default advertised a bare host, so a
// discovery client dialing it landed on 443 and found nothing).
func TestSSDPHostPortOmitsOnlyTheSchemeDefault(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"https on the shipped non-default port", config.Config{Proto: "https", Port: config.Port{HTTPS: 8443}}, "10.42.0.19:8443"},
		{"https on the scheme default is bare", config.Config{Proto: "https", Port: config.Port{HTTPS: 443}}, "10.42.0.19"},
		{"http on a non-default port", config.Config{Proto: "http", Port: config.Port{HTTP: 8080}}, "10.42.0.19:8080"},
		{"http on the scheme default is bare", config.Config{Proto: "http", Port: config.Port{HTTP: 80}}, "10.42.0.19"},
		{"zero port is treated as unset, stays bare", config.Config{Proto: "https", Port: config.Port{HTTPS: 0}}, "10.42.0.19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ssdpHostPort(&tc.cfg, "10.42.0.19"); got != tc.want {
				t.Errorf("ssdpHostPort(%+v) = %q, want %q", tc.cfg, got, tc.want)
			}
		})
	}
}
