package network

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func testRHIServer() *rhiDHCPServer {
	return &rhiDHCPServer{
		iface:    "usb0",
		serverIP: net.IPv4(169, 254, 10, 1),
		leaseIP:  net.IPv4(169, 254, 10, 2),
		mask:     net.CIDRMask(16, 32),
	}
}

func TestRHIDHCPDiscoverGetsOffer(t *testing.T) {
	s := testRHIServer()
	req, _ := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover))

	resp := s.reply(req)
	if resp == nil {
		t.Fatal("no reply to DISCOVER")
	}
	if mt := resp.MessageType(); mt != dhcpv4.MessageTypeOffer {
		t.Errorf("reply type = %s, want OFFER", mt)
	}
	if !resp.YourIPAddr.Equal(s.leaseIP) {
		t.Errorf("yiaddr = %s, want %s", resp.YourIPAddr, s.leaseIP)
	}
	// The whole point of the single-lease server: never a router or DNS.
	if routers := resp.Router(); len(routers) != 0 {
		t.Errorf("offer carries router option %v; must not", routers)
	}
	if dns := resp.DNS(); len(dns) != 0 {
		t.Errorf("offer carries dns option %v; must not", dns)
	}
	if mask := resp.SubnetMask(); mask.String() != s.mask.String() {
		t.Errorf("subnet mask = %s, want %s", mask, s.mask)
	}
}

func TestRHIDHCPRequestAckAndNak(t *testing.T) {
	s := testRHIServer()

	req, _ := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(s.leaseIP)),
	)
	if resp := s.reply(req); resp == nil || resp.MessageType() != dhcpv4.MessageTypeAck {
		t.Errorf("REQUEST for the lease: got %v, want ACK", resp.MessageType())
	}

	stale, _ := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.IPv4(192, 168, 7, 2))),
	)
	if resp := s.reply(stale); resp == nil || resp.MessageType() != dhcpv4.MessageTypeNak {
		t.Errorf("REQUEST for a foreign address: got %v, want NAK", resp.MessageType())
	}
}

func TestRHIDHCPIgnoresNoise(t *testing.T) {
	s := testRHIServer()
	if resp := s.reply(nil); resp != nil {
		t.Error("nil request must be ignored")
	}
	rel, _ := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeRelease))
	if resp := s.reply(rel); resp != nil {
		t.Error("RELEASE must be ignored")
	}
	// A reply (op=BOOTREPLY) must never be answered — e.g. our own broadcast.
	offer, _ := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer))
	offer.OpCode = dhcpv4.OpcodeBootReply
	if resp := s.reply(offer); resp != nil {
		t.Error("BOOTREPLY must be ignored")
	}
}
