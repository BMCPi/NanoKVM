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
	// dhcpMinRetry bounds the exponential backoff between failed acquisitions
	// (growBackoff caps it at supRetryCap); dhcpMinRenew floors the renew timer.
	dhcpMinRetry = 5 * time.Second
	dhcpMinRenew = 30 * time.Second
	// dhcpFallbackLease is used when a server omits the lease-time option.
	dhcpFallbackLease = time.Hour
)

// dhcpRunner drives an in-process DHCPv4 client (insomniacslk/dhcp) for one
// interface: acquire a lease, apply it via netlink, then renew at T1 (50% of the
// lease). This replaces the external udhcpc that the S30eth init script ran.
// Modeled on JetKVM's pkg/nmlite/jetdhcpc usage.
//
// The runner is self-healing: it retries failed acquisitions with backoff,
// re-checks the netdev before every exchange (it can vanish and reappear), and
// wakes on kicks from the link monitor to re-apply a lease the kernel lost
// (link flap, netdev recreation, address flush) — falling back to a full
// reacquisition when re-applying fails.
type dhcpRunner struct {
	iface string
	done  <-chan struct{}
	// kick asks the runner to verify the lease is still programmed on the link.
	kick <-chan struct{}
	// onAttempt is invoked after the first acquisition attempt completes,
	// success or failure (used to release WaitReady).
	onAttempt func()
}

func (d *dhcpRunner) run() {
	var lease *nclient4.Lease
	backoff := dhcpMinRetry

	for {
		// The netdev can vanish and reappear (driver rebind, admin-down); make
		// sure it exists and is up before talking DHCP.
		if err := ensureLinkUp(d.iface); err != nil {
			log.Warnf("network: dhcp %s: %v (retry in %s)", d.iface, err, backoff)
			d.signalAttempt()
			if sleepOrDone(d.done, backoff) {
				return
			}
			backoff = growBackoff(backoff)
			continue
		}

		var err error
		if lease == nil {
			lease, err = dhcpRequest(d.iface, d.done) // full DISCOVER/REQUEST
		} else {
			lease, err = dhcpRenew(d.iface, lease, d.done) // unicast renew
		}
		d.signalAttempt()
		if err != nil {
			select {
			case <-d.done:
				return // exchange cancelled by shutdown
			default:
			}
			log.Warnf("network: dhcp %s: %v (retry in %s)", d.iface, err, backoff)
			lease = nil
			if sleepOrDone(d.done, backoff) {
				return
			}
			backoff = growBackoff(backoff)
			continue
		}
		backoff = dhcpMinRetry

		if err := applyLease(d.iface, lease); err != nil {
			log.Errorf("network: dhcp %s apply lease: %v", d.iface, err)
			lease = nil
			if sleepOrDone(d.done, dhcpMinRetry) {
				return
			}
			continue
		}
		log.Infof("network: dhcp %s bound %s (renew in %s)",
			d.iface, lease.ACK.YourIPAddr, renewAfter(lease))

		stopped, reacquire := d.waitRenew(lease)
		if stopped {
			return
		}
		if reacquire {
			lease = nil
		}
	}
}

func (d *dhcpRunner) signalAttempt() {
	if d.onAttempt != nil {
		d.onAttempt() // sync.Once on the manager side; safe to call repeatedly
	}
}

// waitRenew sleeps until the lease's renewal time, waking on kicks from the
// link monitor to verify the address is still programmed (a link flap or
// netdev re-creation wipes it). Returns stopped=true on shutdown; returns
// reacquire=true when the lease could not be re-applied and a full DISCOVER
// is needed.
func (d *dhcpRunner) waitRenew(lease *nclient4.Lease) (stopped, reacquire bool) {
	timer := time.NewTimer(renewAfter(lease))
	defer timer.Stop()
	for {
		select {
		case <-d.done:
			return true, false
		case <-timer.C:
			return false, false // time to renew
		case <-d.kick:
			if leaseApplied(d.iface, lease) {
				continue
			}
			log.Warnf("network: dhcp %s: address lost; re-applying lease", d.iface)
			if err := applyLease(d.iface, lease); err != nil {
				log.Warnf("network: dhcp %s: re-apply failed (%v); reacquiring", d.iface, err)
				return false, true
			}
		}
	}
}

// leaseApplied reports whether the lease's address is still programmed on an
// up interface.
func leaseApplied(iface string, lease *nclient4.Lease) bool {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return false
	}
	if !isAdminUp(link) {
		return false
	}
	ipnet, _, _, err := leaseAddrs(lease.ACK)
	if err != nil {
		return false
	}
	return hasAddr(link, &netlink.Addr{IPNet: ipnet})
}

// exchangeContext bounds a DHCP transaction and cancels it early when the
// manager shuts down, so Stop/Restart never block behind an in-flight exchange.
func exchangeContext(done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), dhcpOverallTimeout)
	if done != nil {
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

func dhcpRequest(iface string, done <-chan struct{}) (*nclient4.Lease, error) {
	c, err := nclient4.New(iface, nclient4.WithTimeout(dhcpExchangeTimeout))
	if err != nil {
		return nil, fmt.Errorf("dhcp client: %w", err)
	}
	defer c.Close()

	ctx, cancel := exchangeContext(done)
	defer cancel()
	return c.Request(ctx)
}

func dhcpRenew(iface string, prev *nclient4.Lease, done <-chan struct{}) (*nclient4.Lease, error) {
	c, err := nclient4.New(iface, nclient4.WithTimeout(dhcpExchangeTimeout))
	if err != nil {
		return nil, fmt.Errorf("dhcp client: %w", err)
	}
	defer c.Close()

	ctx, cancel := exchangeContext(done)
	defer cancel()
	return c.Renew(ctx, prev)
}

// renewAfter returns T1 (half the lease time, RFC 2131), floored so a tiny lease
// does not spin.
func renewAfter(lease *nclient4.Lease) time.Duration {
	return max(lease.ACK.IPAddressLeaseTime(dhcpFallbackLease)/2, dhcpMinRenew)
}

// applyLease programs the interface from a DHCP ACK: link up, address, default
// route and resolv.conf, all via netlink.
func applyLease(iface string, lease *nclient4.Lease) error {
	ipnet, gw, dns, err := leaseAddrs(lease.ACK)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("link %s: %w", iface, err)
	}
	if err := ensureUp(link); err != nil {
		return err
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
