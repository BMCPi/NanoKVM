package discovery

import (
	"reflect"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// TestLocalNames moved verbatim from pkg/mdns/mdns_test.go — the hostname
// normalization it pins did not change when the responders it feeds did.
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

	r, err := Start()
	if err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	if r != nil {
		t.Fatal("Start() returned a non-nil Responder with both protocols disabled")
	}
}
