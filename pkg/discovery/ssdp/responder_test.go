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

func TestStopIsIdempotent(t *testing.T) {
	r := New(Config{Iface: "lo"})
	r.Stop()
	r.Stop() // must not panic
}
