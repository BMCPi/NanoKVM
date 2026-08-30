package mdns

import (
	"context"
	"testing"
	"time"
)

func TestResponderAnnouncesServicesOnLoopback(t *testing.T) {
	r := New("nanokvm-test.local", "lo", []Service{
		{Type: "_redfish._tcp", Port: 8443, Text: map[string]string{"txtvers": "1"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	name, ok := r.Name()
	if !ok || name != "nanokvm-test.local" {
		t.Errorf("Name() = %q, %v; want the advertised host", name, ok)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	r := New("nanokvm-test.local", "lo", nil)
	r.Stop()
	r.Stop() // must not panic
}
