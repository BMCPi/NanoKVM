package ssdp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestResponderAnswersMSearch(t *testing.T) {
	r := New(Config{
		Iface: "lo", UUID: "aaaa-bbbb",
		Location: "https://127.0.0.1/redfish/v1/", Minor: 13, MaxAge: 1800,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	search := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\nMX: 1\r\n" +
		"ST: urn:dmtf-org:service:redfish-rest:1\r\n\r\n")
	if _, err := conn.WriteTo(search, group); err != nil {
		t.Skipf("cannot send to the SSDP group here: %v", err)
	}

	buf := make([]byte, 2048)
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no answer to M-SEARCH: %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "AL: https://127.0.0.1/redfish/v1/") {
		t.Errorf("answer missing our AL header:\n%s", got)
	}
}

func TestStopIsIdempotent(_ *testing.T) {
	r := New(Config{Iface: "lo"})
	r.Stop()
	r.Stop() // must not panic
}

// TestStopSendsByebyeBeforeClosingSocket pins the ordering Stop's comment
// promises: ssdp:byebye must reach the wire before the socket that would
// carry it closes. Swapping sendNotify and conn.Close in Stop makes the
// write land on an already-closed fd — WriteToUDP then errors and the error
// is discarded, so the goodbye silently never sends and this test times out
// waiting for it. Verified by inverting the two lines locally and observing
// the failure (see fix-review-findings-report.md).
func TestStopSendsByebyeBeforeClosingSocket(t *testing.T) {
	ifi, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skipf("no lo interface: %v", err)
	}
	// Join the group before Start so the initial ssdp:alive announce() fires
	// immediately on a running responder can't be sent and missed before
	// this listener exists.
	listener, err := net.ListenMulticastUDP("udp4", ifi, ssdpGroup)
	if err != nil {
		t.Skipf("cannot join the SSDP group on lo: %v", err)
	}
	defer listener.Close()

	r := New(Config{
		Iface: "lo", UUID: "cccc-dddd",
		Location: "https://127.0.0.1/redfish/v1/", Minor: 13, MaxAge: 1800,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}

	// Drain the ssdp:alive announce() fires on Start so it can't be
	// mistaken for the byebye this test actually watches for.
	buf := make([]byte, 2048)
	_ = listener.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		n, _, err := listener.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no ssdp:alive seen before Stop: %v", err)
		}
		if strings.Contains(string(buf[:n]), "ssdp:alive") {
			break
		}
	}

	r.Stop()

	_ = listener.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no ssdp:byebye received after Stop (inverting Stop's send/close order drops it silently): %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "ssdp:byebye") {
		t.Errorf("expected ssdp:byebye, got:\n%s", got)
	}
}

// TestReplyDelaySpansTheFullMXWindow guards against reply drawing a whole
// number of seconds (rand.Int63n(int64(search.MX))) instead of a duration
// across the whole window (rand.Int63n(int64(MX*time.Second))): with the
// former, MX=1 — what a missing or unparseable MX header clamps to, so most
// real searches — always drew exactly 0s, defeating the delay's purpose of
// spreading out a fleet's simultaneous replies. This sends many MX=1
// searches and requires seeing delays spread across the sub-second range,
// which the whole-seconds bug could never produce for MX=1.
func TestReplyDelaySpansTheFullMXWindow(t *testing.T) {
	r := New(Config{
		Iface: "lo", UUID: "1111-2222",
		Location: "https://127.0.0.1/redfish/v1/", Minor: 13, MaxAge: 1800,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Skipf("no multicast on this host: %v", err)
	}
	defer r.Stop()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	search := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\nMX: 1\r\n" +
		"ST: urn:dmtf-org:service:redfish-rest:1\r\n\r\n")

	const trials = 30
	var sawOverHalfSecond bool
	buf := make([]byte, 2048)
	for i := 0; i < trials; i++ {
		start := time.Now()
		if _, err := conn.WriteTo(search, group); err != nil {
			t.Skipf("cannot send to the SSDP group here: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := conn.ReadFrom(buf); err != nil {
			t.Fatalf("no answer to M-SEARCH: %v", err)
		}
		if time.Since(start) > 500*time.Millisecond {
			sawOverHalfSecond = true
			break
		}
	}
	if !sawOverHalfSecond {
		t.Errorf("all %d MX=1 replies arrived within 500ms; delay looks clamped to whole seconds (always 0s), not drawn across the full MX window", trials)
	}
}

// TestStopDoesNotClearANewerGeneration is a deterministic (no scheduler
// luck needed) reproduction of FINDING 1. Forcing the exact interleaving
// the finding describes by racing two bare goroutines is unreliable in
// practice: Stop's fast nil-check path almost always wins before a
// concurrent Start can finish replacing the generation, so the bug's window
// is too narrow to hit reliably even at thousands of iterations. Instead
// this test controls the timing directly: it pauses a generation's cancel
// mid-teardown (same package, so the private generation type and r.gen/r.mu
// are reachable directly), installs a second generation while the first
// Stop() call is paused there — reproducing "a concurrent Start() runs to
// completion while Stop() is still holding its stale snapshot" exactly —
// then lets it resume. Before the fix, Stop's trailing clear ran
// unconditionally on r.gen and would have wiped out the second generation
// this test installed out from under it.
func TestStopDoesNotClearANewerGeneration(t *testing.T) {
	r := &Responder{}

	conn1, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn1.Close()

	pausedInCancel := make(chan struct{})
	resumeCancel := make(chan struct{})
	g1 := &generation{
		conn: conn1,
		cancel: func() {
			close(pausedInCancel)
			<-resumeCancel
		},
	}

	r.mu.Lock()
	r.gen = g1
	r.mu.Unlock()

	stopDone := make(chan struct{})
	go func() {
		r.Stop()
		close(stopDone)
	}()

	// Wait until Stop() has snapshotted g1 and is paused inside its
	// teardown — i.e. exactly the moment a concurrent Start() would, in the
	// real bug, run to completion behind Stop's back.
	<-pausedInCancel

	conn2, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn2.Close()
	g2 := &generation{conn: conn2, cancel: func() {}}

	r.mu.Lock()
	r.gen = g2
	r.mu.Unlock()

	close(resumeCancel)
	<-stopDone

	r.mu.Lock()
	got := r.gen
	r.mu.Unlock()
	if got != g2 {
		t.Fatalf("Stop() clobbered a generation it never snapshotted: r.gen = %p, want untouched g2 = %p", got, g2)
	}
}
