package network

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

const (
	// dhcpExchangeTimeout bounds a single packet exchange; dhcpOverallTimeout
	// bounds the whole DISCOVER/REQUEST (or renew) transaction.
	dhcpExchangeTimeout = 10 * time.Second
	dhcpOverallTimeout  = 30 * time.Second
	// dhcpMinRetry/dhcpMaxRetry bound the exponential backoff between failed
	// acquisitions; dhcpMinRenew floors the renew timer.
	dhcpMinRetry = 5 * time.Second
	dhcpMaxRetry = 60 * time.Second
	dhcpMinRenew = 30 * time.Second
	// dhcpFallbackLease is used when a server omits the lease-time option.
	dhcpFallbackLease = time.Hour
)

// dhcpRunner drives an in-process DHCPv4 client (insomniacslk/dhcp) for one
// interface: acquire a lease, apply it via netlink, then renew at T1 (50% of the
// lease). This replaces the external udhcpc that the S30eth init script ran.
// Modeled on JetKVM's pkg/nmlite/jetdhcpc usage.
type dhcpRunner struct {
	iface string
	done  <-chan struct{}
}

func (d *dhcpRunner) run() {
	var lease *nclient4.Lease
	backoff := dhcpMinRetry

	for {
		var err error
		if lease == nil {
			lease, err = dhcpRequest(d.iface) // full DISCOVER/REQUEST
		} else {
			lease, err = dhcpRenew(d.iface, lease) // unicast renew
		}
		if err != nil {
			log.Warnf("network: dhcp %s: %v (retry in %s)", d.iface, err, backoff)
			lease = nil
			if sleepOrDone(d.done, backoff) {
				return
			}
			if backoff < dhcpMaxRetry {
				backoff *= 2
			}
			continue
		}
		backoff = dhcpMinRetry

		if err := applyLease(d.iface, lease); err != nil {
			log.Errorf("network: dhcp %s apply lease: %v", d.iface, err)
		} else {
			log.Infof("network: dhcp %s bound %s (renew in %s)",
				d.iface, lease.ACK.YourIPAddr, renewAfter(lease))
		}
		if sleepOrDone(d.done, renewAfter(lease)) {
			return
		}
	}
}

func dhcpRequest(iface string) (*nclient4.Lease, error) {
	c, err := nclient4.New(iface, nclient4.WithTimeout(dhcpExchangeTimeout))
	if err != nil {
		return nil, fmt.Errorf("dhcp client: %w", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), dhcpOverallTimeout)
	defer cancel()
	return c.Request(ctx)
}

func dhcpRenew(iface string, prev *nclient4.Lease) (*nclient4.Lease, error) {
	c, err := nclient4.New(iface, nclient4.WithTimeout(dhcpExchangeTimeout))
	if err != nil {
		return nil, fmt.Errorf("dhcp client: %w", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), dhcpOverallTimeout)
	defer cancel()
	return c.Renew(ctx, prev)
}

// renewAfter returns T1 (half the lease time, RFC 2131), floored so a tiny lease
// does not spin.
func renewAfter(lease *nclient4.Lease) time.Duration {
	renew := lease.ACK.IPAddressLeaseTime(dhcpFallbackLease) / 2
	if renew < dhcpMinRenew {
		renew = dhcpMinRenew
	}
	return renew
}

// applyLease programs the interface from a DHCP ACK: address, default route and
// resolv.conf, all via netlink.
func applyLease(iface string, lease *nclient4.Lease) error {
	ipnet, gw, dns, err := leaseAddrs(lease.ACK)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("link %s: %w", iface, err)
	}
	if err := replaceAddr(link, &netlink.Addr{IPNet: ipnet}); err != nil {
		return err
	}
	if gw != nil {
		if err := replaceDefaultRoute(link, gw); err != nil {
			log.Warnf("network: %v", err)
		}
	}
	writeResolvConf(dns)
	return nil
}

// leaseAddrs extracts the address/mask, default gateway and DNS servers from a
// DHCP ACK. Pure (no netlink) so it is unit-testable. A missing subnet mask
// falls back to /24, matching common server behavior when the option is absent.
func leaseAddrs(ack *dhcpv4.DHCPv4) (*net.IPNet, net.IP, []net.IP, error) {
	if ack == nil {
		return nil, nil, nil, fmt.Errorf("dhcp ack is nil")
	}
	ip := ack.YourIPAddr.To4()
	if ip == nil || ip.IsUnspecified() {
		return nil, nil, nil, fmt.Errorf("dhcp ack has no yiaddr")
	}
	mask := ack.SubnetMask()
	if len(mask) == 0 {
		mask = net.CIDRMask(24, 32)
	}
	ipnet := &net.IPNet{IP: ip, Mask: mask}

	var gw net.IP
	if routers := ack.Router(); len(routers) > 0 {
		gw = routers[0].To4()
	}
	return ipnet, gw, ack.DNS(), nil
}

func sleepOrDone(done <-chan struct{}, d time.Duration) (stopped bool) {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
