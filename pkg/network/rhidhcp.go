package network

import (
	"log/slog"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// rhiLeaseTime matches the old udhcpd-usb0.conf lease option.
const rhiLeaseTime = time.Hour

// rhiDHCPServer is a deliberately tiny in-process DHCPv4 server for the
// point-to-point USB host link, replacing the single-lease udhcpd the build's
// ifupdown hook ran. It hands the single peer one fixed address with subnet
// mask and lease time only — no router, DNS or domain options — so a host
// DHCP client gets a working peer address without ever learning a route or
// resolver through the BMC (Redfish-Host-Interface style, DSP0270). Hosts
// with no DHCP client still work: they self-assign an APIPA address in the
// same /16.
type rhiDHCPServer struct {
	iface    string
	serverIP net.IP // BMC side, e.g. 169.254.10.1
	leaseIP  net.IP // host side, e.g. 169.254.10.2
	mask     net.IPMask
	srv      *server4.Server
	log      *slog.Logger
}

// startRHIDHCP binds :67 on the given interface and serves in a goroutine.
func startRHIDHCP(iface string, serverIP, leaseIP net.IP, mask net.IPMask, log *slog.Logger) (*rhiDHCPServer, error) {
	s := &rhiDHCPServer{iface: iface, serverIP: serverIP, leaseIP: leaseIP, mask: mask, log: log}
	srv, err := server4.NewServer(iface, &net.UDPAddr{Port: 67}, s.handle)
	if err != nil {
		return nil, err
	}
	s.srv = srv
	go func() {
		// Serve returns on Close or on a socket error (e.g. the netdev was
		// torn down); the next RHI (re)configure starts a fresh server.
		if err := srv.Serve(); err != nil {
			s.log.Debug("network: RHI dhcp server exited", slog.String("iface", iface), slog.Any("err", err))
		}
	}()
	s.log.Info("network: RHI dhcp server started", slog.String("iface", iface), slog.Any("lease", leaseIP))
	return s, nil
}

func (s *rhiDHCPServer) stop() {
	_ = s.srv.Close()
}

func (s *rhiDHCPServer) handle(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	resp := s.reply(req)
	if resp == nil {
		return
	}
	if _, err := conn.WriteTo(resp.ToBytes(), peer); err != nil {
		s.log.Debug("network: RHI dhcp reply failed", slog.Any("err", err))
	}
}

// reply builds the response for a request, or nil to stay silent. Pure (no
// I/O) so it is unit-testable. There is exactly one peer on this link, so
// every DISCOVER is offered the fixed lease; a REQUEST for anything else is
// NAKed, which makes a client with stale state re-discover.
func (s *rhiDHCPServer) reply(req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	if req == nil || req.OpCode != dhcpv4.OpcodeBootRequest {
		return nil
	}
	var respType dhcpv4.MessageType
	switch req.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		respType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		respType = dhcpv4.MessageTypeAck
		if r := req.RequestedIPAddress(); r != nil && !r.IsUnspecified() && !r.Equal(s.leaseIP) {
			respType = dhcpv4.MessageTypeNak
		} else if ci := req.ClientIPAddr; ci != nil && !ci.IsUnspecified() && !ci.Equal(s.leaseIP) {
			respType = dhcpv4.MessageTypeNak
		}
	default:
		return nil
	}

	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(respType),
		dhcpv4.WithServerIP(s.serverIP),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.serverIP)),
	}
	if respType != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithYourIP(s.leaseIP),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.mask)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(rhiLeaseTime)),
		)
	}
	resp, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		s.log.Debug("network: RHI dhcp reply build failed", slog.Any("err", err))
		return nil
	}
	return resp
}
