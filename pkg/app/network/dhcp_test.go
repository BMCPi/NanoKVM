package network

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

// discardRunner builds a dhcpRunner for the given interface with a
// discard-handler logger, for tests that exercise a method requiring one but
// do not care about its output.
func discardRunner(iface string) *dhcpRunner {
	return &dhcpRunner{iface: iface, log: slog.New(slog.DiscardHandler)}
}

// leaseWith builds a lease created at a fixed instant, optionally carrying
// explicit T1/T2 options, so the derived instants are exactly predictable.
func leaseWith(t *testing.T, life, t1, t2 time.Duration) (*nclient4.Lease, time.Time) {
	t.Helper()
	ack, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("dhcpv4.New: %v", err)
	}
	ack.YourIPAddr = net.IPv4(192, 168, 1, 50)
	if life > 0 {
		ack.UpdateOption(dhcpv4.OptIPAddressLeaseTime(life))
	}
	if t1 > 0 {
		ack.UpdateOption(dhcpv4.OptRenewTimeValue(t1))
	}
	if t2 > 0 {
		ack.UpdateOption(dhcpv4.OptRebindingTimeValue(t2))
	}
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	return &nclient4.Lease{ACK: ack, CreationTime: base}, base
}

func TestLeaseTimesDefaultFractions(t *testing.T) {
	// No option 58/59: RFC 2131's 0.5 and 0.875 of the lease time.
	lease, base := leaseWith(t, time.Hour, 0, 0)
	t1, t2, expiry := leaseTimes(lease)

	if want := base.Add(30 * time.Minute); !t1.Equal(want) {
		t.Errorf("T1 = %s, want %s", t1, want)
	}
	if want := base.Add(52*time.Minute + 30*time.Second); !t2.Equal(want) {
		t.Errorf("T2 = %s, want %s", t2, want)
	}
	if want := base.Add(time.Hour); !expiry.Equal(want) {
		t.Errorf("expiry = %s, want %s", expiry, want)
	}
}

func TestLeaseTimesHonoursServerValues(t *testing.T) {
	// Explicit T1/T2 from the server win over the default fractions.
	lease, base := leaseWith(t, time.Hour, 10*time.Minute, 40*time.Minute)
	t1, t2, expiry := leaseTimes(lease)

	if want := base.Add(10 * time.Minute); !t1.Equal(want) {
		t.Errorf("T1 = %s, want %s (server option 58)", t1, want)
	}
	if want := base.Add(40 * time.Minute); !t2.Equal(want) {
		t.Errorf("T2 = %s, want %s (server option 59)", t2, want)
	}
	if want := base.Add(time.Hour); !expiry.Equal(want) {
		t.Errorf("expiry = %s, want %s", expiry, want)
	}
}

func TestLeaseTimesStrictlyIncreasing(t *testing.T) {
	// Degenerate inputs must still yield T1 < T2 < expiry, or maintain() would
	// skip renewal entirely and spin on re-acquisition.
	for _, tc := range []struct {
		name           string
		life, t1v, t2v time.Duration
	}{
		{"tiny lease under the renew floor", 10 * time.Second, 0, 0},
		{"server inverts T1 and T2", time.Hour, 50 * time.Minute, 10 * time.Minute},
		{"T2 beyond the lease", time.Hour, 10 * time.Minute, 90 * time.Minute},
		{"no lease time at all", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease, _ := leaseWith(t, tc.life, tc.t1v, tc.t2v)
			t1, t2, expiry := leaseTimes(lease)
			if !t1.Before(t2) {
				t.Errorf("T1 %s not before T2 %s", t1, t2)
			}
			if !t2.Before(expiry) {
				t.Errorf("T2 %s not before expiry %s", t2, expiry)
			}
		})
	}
}

func TestRememberedAddrRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := dhcpLeaseDir
	dhcpLeaseDir = dir
	defer func() { dhcpLeaseDir = old }()

	d := discardRunner("eth0")

	if got := loadRememberedAddr("eth0"); got != nil {
		t.Errorf("loadRememberedAddr with no file = %v, want nil", got)
	}

	d.rememberAddr(net.IPv4(10, 0, 92, 76))
	got := loadRememberedAddr("eth0")
	if got == nil || !got.Equal(net.IPv4(10, 0, 92, 76)) {
		t.Fatalf("loadRememberedAddr = %v, want 10.0.92.76", got)
	}

	// An unspecified address is not worth remembering, and must not clobber a
	// good one already on disk.
	d.rememberAddr(net.IPv4zero)
	if got := loadRememberedAddr("eth0"); got == nil || !got.Equal(net.IPv4(10, 0, 92, 76)) {
		t.Errorf("after remembering 0.0.0.0, got %v, want the previous 10.0.92.76", got)
	}

	// Garbage on disk reads as "nothing remembered" rather than a bad request.
	if err := os.WriteFile(filepath.Join(dir, "dhcp-eth0.addr"), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadRememberedAddr("eth0"); got != nil {
		t.Errorf("loadRememberedAddr on garbage = %v, want nil", got)
	}
}

func TestForgetRememberedAddr(t *testing.T) {
	dir := t.TempDir()
	old := dhcpLeaseDir
	dhcpLeaseDir = dir
	defer func() { dhcpLeaseDir = old }()

	d := discardRunner("eth0")

	d.rememberAddr(net.IPv4(10, 42, 0, 204))
	if loadRememberedAddr("eth0") == nil {
		t.Fatal("setup: address was not remembered")
	}

	// An address no server will confirm has to be dropped, or it costs an
	// INIT-REBOOT attempt on every start for the life of the file.
	d.forgetRememberedAddr()
	if got := loadRememberedAddr("eth0"); got != nil {
		t.Errorf("after forget, loadRememberedAddr = %v, want nil", got)
	}

	// Forgetting what is already gone is not an error.
	d.forgetRememberedAddr()
}

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
	m := &Manager{log: slog.New(slog.DiscardHandler)}
	got := m.parseDNS([]string{"8.8.8.8", "not-an-ip", "1.1.1.1", ""})
	if len(got) != 2 {
		t.Fatalf("parseDNS returned %d servers, want 2 (invalids dropped): %v", len(got), got)
	}
	if !got[0].Equal(net.IPv4(8, 8, 8, 8)) || !got[1].Equal(net.IPv4(1, 1, 1, 1)) {
		t.Errorf("parseDNS = %v, want [8.8.8.8 1.1.1.1]", got)
	}
}

// --- broadcast flag ------------------------------------------------------------

var testHW = net.HardwareAddr{0x02, 0x00, 0x5e, 0x10, 0x00, 0x01}

// buildDiscover reproduces exactly what nclient4.DiscoverOffer puts on the
// wire for a given modifier set, so these tests exercise the real composition
// (our modifiers are appended last and therefore win) rather than asserting on
// the slice we hand over.
func buildDiscover(t *testing.T, mods []dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	pkt, err := dhcpv4.NewDiscovery(testHW, dhcpv4.PrependModifiers(mods,
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(nclient4.MaxMessageSize)))...)
	if err != nil {
		t.Fatalf("build discover: %v", err)
	}
	return pkt
}

// A client in SELECTING state has no address. Without the broadcast flag the
// server may unicast the offer to an address the client has not configured,
// which a relay agent -- the normal arrangement when a managed switch serves
// DHCP from another VLAN -- cannot deliver, because it has no way to ARP for
// the host. Flat segments hide this; relayed ones do not.
func TestDiscoverAsksForABroadcastReply(t *testing.T) {
	d := &dhcpRunner{}
	if got := buildDiscover(t, d.discoverOptions(testHW)); !got.IsBroadcast() {
		t.Errorf("DISCOVER flags = %#x, want the broadcast bit set", got.Flags)
	}
}

// nclient4.Request passes the same modifiers to the REQUEST it builds from the
// offer. RFC 2131 4.4.1 wants that REQUEST to carry the same flag as the
// DISCOVER it followed, or the server switches delivery mode mid-exchange.
func TestRequestFromOfferKeepsTheBroadcastFlag(t *testing.T) {
	offer, err := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
		dhcpv4.WithHwAddr(testHW), dhcpv4.WithYourIP(net.IPv4(192, 0, 2, 50)))
	if err != nil {
		t.Fatalf("build offer: %v", err)
	}
	d := &dhcpRunner{}
	req, err := dhcpv4.NewRequestFromOffer(offer, dhcpv4.PrependModifiers(d.discoverOptions(testHW),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(nclient4.MaxMessageSize)))...)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if !req.IsBroadcast() {
		t.Errorf("REQUEST flags = %#x, want the broadcast bit set", req.Flags)
	}
}

// RENEW goes unicast to the leasing server (RFC 2131 4.3.6) from a client that
// already holds the address. Broadcasting it would be wrong, and is exactly
// what putting the flag in options() rather than discoverOptions() would cause.
func TestRenewStaysUnicast(t *testing.T) {
	ack, err := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithHwAddr(testHW), dhcpv4.WithYourIP(net.IPv4(192, 0, 2, 50)))
	if err != nil {
		t.Fatalf("build ack: %v", err)
	}
	d := &dhcpRunner{}
	renew, err := dhcpv4.NewRenewFromAck(ack, d.options(testHW)...)
	if err != nil {
		t.Fatalf("build renew: %v", err)
	}
	if renew.IsBroadcast() {
		t.Errorf("RENEW flags = %#x, want unicast", renew.Flags)
	}
}

// The broadcast flag must not have displaced what every message already
// carried: a type-1 client identifier built from the hardware address.
func TestDiscoverStillCarriesTheClientIdentifier(t *testing.T) {
	d := &dhcpRunner{}
	pkt := buildDiscover(t, d.discoverOptions(testHW))

	want := append([]byte{0x01}, testHW...)
	if got := pkt.Options.Get(dhcpv4.OptionClientIdentifier); !bytes.Equal(got, want) {
		t.Errorf("client identifier = %v, want %v", got, want)
	}
	if !pkt.IsBroadcast() {
		t.Error("broadcast flag lost")
	}
}

// An interface with no hardware address must not produce a malformed
// client-id option; the flag is still owed.
func TestDiscoverWithoutHardwareAddress(t *testing.T) {
	d := &dhcpRunner{}
	pkt := buildDiscover(t, d.discoverOptions(nil))

	if got := pkt.Options.Get(dhcpv4.OptionClientIdentifier); got != nil {
		t.Errorf("client identifier = %v, want none when the MAC is unknown", got)
	}
	if !pkt.IsBroadcast() {
		t.Error("broadcast flag lost")
	}
}

// --- carrier-driven reacquire ---------------------------------------------------

func leaseFor(t *testing.T, ip net.IP) *nclient4.Lease {
	t.Helper()
	ack, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithHwAddr(testHW),
		dhcpv4.WithYourIP(ip),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour)),
	)
	if err != nil {
		t.Fatalf("build ack: %v", err)
	}
	return &nclient4.Lease{ACK: ack}
}

// A carrier event says the link came back, not that the binding is gone. The
// runner must re-request the address it was holding (INIT-REBOOT) instead of
// discarding it, or a link flap silently moves the BMC's management address --
// and on a segment offering more than one subnet, it does: observed hopping
// 10.0.199.135 -> 10.1.9.89 -> 10.0.199.135 across three re-acquisitions.
func TestReacquireKeepsTheAddressToReRequest(t *testing.T) {
	want := net.IPv4(10, 0, 199, 135)
	reacquire := make(chan struct{}, 1)
	reacquire <- struct{}{} // carrier came back while we held the lease

	d := &dhcpRunner{iface: "lo", reacquire: reacquire, log: discardLog()}
	lease := leaseFor(t, want)

	outcome, back := d.maintain(lease)

	if outcome != dhcpReacquired {
		t.Fatalf("outcome = %v, want dhcpReacquired", outcome)
	}
	if got := leaseAddr(back); !got.Equal(want) {
		t.Errorf("handed back %v, want the held address %v", got, want)
	}
}

// dhcpExpired and dhcpReacquired must stay distinct: only the latter may
// re-request the old address. Collapsing them is what a bare DISCOVER after a
// flap looks like.
func TestReacquireIsDistinctFromExpired(t *testing.T) {
	if dhcpReacquired == dhcpExpired {
		t.Fatal("dhcpReacquired and dhcpExpired are the same value")
	}
}

func TestLeaseAddr(t *testing.T) {
	if got := leaseAddr(nil); got != nil {
		t.Errorf("leaseAddr(nil) = %v, want nil", got)
	}
	if got := leaseAddr(&nclient4.Lease{}); got != nil {
		t.Errorf("leaseAddr(no ACK) = %v, want nil", got)
	}
	if got := leaseAddr(leaseFor(t, net.IPv4zero)); got != nil {
		t.Errorf("leaseAddr(0.0.0.0) = %v, want nil — there is nothing to reclaim", got)
	}
	want := net.IPv4(10, 1, 9, 89)
	if got := leaseAddr(leaseFor(t, want)); !got.Equal(want) {
		t.Errorf("leaseAddr = %v, want %v", got, want)
	}
}
