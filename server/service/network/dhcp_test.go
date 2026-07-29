package network

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestLeaseAddrsFull(t *testing.T) {
	ack, _ := dhcpv4.New()
	ack.YourIPAddr = net.IPv4(192, 168, 1, 50)
	ack.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32)))
	ack.UpdateOption(dhcpv4.OptRouter(net.IPv4(192, 168, 1, 1)))
	ack.UpdateOption(dhcpv4.OptDNS(net.IPv4(8, 8, 8, 8), net.IPv4(1, 1, 1, 1)))

	ipnet, gw, dns, err := leaseAddrs(ack)
	if err != nil {
		t.Fatalf("leaseAddrs: %v", err)
	}
	if got := ipnet.String(); got != "192.168.1.50/24" {
		t.Errorf("ipnet = %s, want 192.168.1.50/24", got)
	}
	if !gw.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Errorf("gw = %s, want 192.168.1.1", gw)
	}
	if len(dns) != 2 || !dns[0].Equal(net.IPv4(8, 8, 8, 8)) || !dns[1].Equal(net.IPv4(1, 1, 1, 1)) {
		t.Errorf("dns = %v, want [8.8.8.8 1.1.1.1]", dns)
	}
}

func TestLeaseAddrsMaskFallback(t *testing.T) {
	// A server that omits the subnet-mask option: fall back to /24.
	ack, _ := dhcpv4.New()
	ack.YourIPAddr = net.IPv4(10, 0, 0, 5)

	ipnet, gw, _, err := leaseAddrs(ack)
	if err != nil {
		t.Fatalf("leaseAddrs: %v", err)
	}
	if got := ipnet.String(); got != "10.0.0.5/24" {
		t.Errorf("ipnet = %s, want 10.0.0.5/24 (mask fallback)", got)
	}
	if gw != nil {
		t.Errorf("gw = %s, want nil (no router option)", gw)
	}
}

func TestLeaseAddrsNoAddress(t *testing.T) {
	ack, _ := dhcpv4.New() // YourIPAddr unset (0.0.0.0)
	if _, _, _, err := leaseAddrs(ack); err == nil {
		t.Fatal("leaseAddrs: expected error for a lease with no yiaddr")
	}
	if _, _, _, err := leaseAddrs(nil); err == nil {
		t.Fatal("leaseAddrs(nil): expected error")
	}
}

func TestParseDNS(t *testing.T) {
	got := parseDNS([]string{"8.8.8.8", "not-an-ip", "1.1.1.1", ""})
	if len(got) != 2 {
		t.Fatalf("parseDNS returned %d servers, want 2 (invalids dropped): %v", len(got), got)
	}
	if !got[0].Equal(net.IPv4(8, 8, 8, 8)) || !got[1].Equal(net.IPv4(1, 1, 1, 1)) {
		t.Errorf("parseDNS = %v, want [8.8.8.8 1.1.1.1]", got)
	}
}
