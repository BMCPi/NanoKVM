package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

const (
	// dhcpExchangeTimeout bounds a single packet exchange; dhcpOverallTimeout
	// bounds the whole DISCOVER/REQUEST (or renew) transaction.
	dhcpExchangeTimeout = 10 * time.Second
	dhcpOverallTimeout  = 30 * time.Second
	// dhcpRebootTimeout bounds the INIT-REBOOT attempt, which is only an
	// optimisation: if no server confirms the remembered address promptly,
	// DISCOVER will. Giving it the full transaction timeout meant a remembered
	// address from another network -- the interface having been moved, or the
	// board bench-tested elsewhere -- stalled every startup for 30s before any
	// useful DHCP happened.
	dhcpRebootTimeout = 4 * time.Second
	// dhcpMinRetry bounds the exponential backoff between failed acquisitions
	// (growBackoff caps it at supRetryCap); dhcpMinRenew floors the renew timer.
	dhcpMinRetry = 5 * time.Second
	dhcpMinRenew = 30 * time.Second
	// dhcpFallbackLease is used when a server omits the lease-time option.
	dhcpFallbackLease = time.Hour
	// dhcpExtendRetry paces RENEW/REBIND attempts within a lease's lifetime.
	// RFC 2131 4.4.5 halves the remaining time down to a 60s floor; a flat,
	// shorter interval is simpler and strictly more eager, which is what a BMC
	// wants — the address matters more than the packet count.
	dhcpExtendRetry = 30 * time.Second
)

// dhcpLeaseDir holds the remembered address per interface, on the persistent
// data partition, so a restart can ask for the same address back (INIT-REBOOT)
// rather than taking whatever DISCOVER happens to offer. A variable only so
// tests can point it at a temporary directory.
var dhcpLeaseDir = "/var/lib/nanokvm"

// dhcpOutcome says why maintain() gave up on the lease it was holding.
type dhcpOutcome int

const (
	dhcpStopped dhcpOutcome = iota // shutting down
	dhcpRenewed                    // fresh ACK in hand; re-apply and keep going
	dhcpExpired                    // binding gone; start again from DISCOVER
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
	// ctx roots every DHCP exchange (see exchangeContext); it is the
	// manager's own ctx, cancelled by Manager.stop(), so a transaction is
	// aborted the moment the manager shuts down instead of running to its
	// own timeout.
	ctx  context.Context
	done <-chan struct{}
	// kick asks the runner to verify the lease is still programmed on the link.
	kick <-chan struct{}
	// reacquire asks the runner to discard the lease and start a fresh
	// DISCOVER. Raised whenever eth0 gains carrier, so a bring-up always asks
	// the server again instead of assuming an address it was holding is still
	// valid -- the link may have come back on a different network.
	reacquire <-chan struct{}
	// onAttempt is invoked after the first acquisition attempt completes,
	// success or failure (used to release WaitReady).
	onAttempt func()

	log *slog.Logger
}

func (d *dhcpRunner) run() {
	// A remembered address from a previous run is worth one INIT-REBOOT attempt.
	remembered := loadRememberedAddr(d.iface)
	var lease *nclient4.Lease
	backoff := dhcpMinRetry

	for {
		// The netdev can vanish and reappear (driver rebind, admin-down); make
		// sure it exists and is up before talking DHCP.
		if err := ensureLinkUp(d.iface); err != nil {
			d.log.Warn("network: dhcp link not ready", slog.String("iface", d.iface), slog.Any("err", err), slog.Duration("retryIn", backoff))
			d.signalAttempt()
			if d.waitBackoff(backoff) {
				return
			}
			backoff = growBackoff(backoff)
			continue
		}

		if lease == nil {
			var err error
			lease, err = d.obtain(remembered)
			remembered = nil // one shot; a failed confirm means it is stale
			d.signalAttempt()
			if err != nil {
				select {
				case <-d.done:
					return // exchange cancelled by shutdown
				default:
				}
				d.log.Warn("network: dhcp acquisition failed", slog.String("iface", d.iface), slog.Any("err", err), slog.Duration("retryIn", backoff))
				if d.waitBackoff(backoff) {
					return
				}
				backoff = growBackoff(backoff)
				continue
			}
			backoff = dhcpMinRetry
		}

		if err := d.applyLease(lease); err != nil {
			d.log.Error("network: dhcp apply lease failed", slog.String("iface", d.iface), slog.Any("err", err))
			lease = nil
			if d.waitBackoff(dhcpMinRetry) {
				return
			}
			continue
		}
		t1, t2, expiry := leaseTimes(lease)
		d.rememberAddr(lease.ACK.YourIPAddr)
		d.log.Info("network: dhcp bound",
			slog.String("iface", d.iface), slog.Any("addr", lease.ACK.YourIPAddr),
			slog.Duration("renewIn", until(t1)), slog.Duration("rebindIn", until(t2)),
			slog.Duration("expiresIn", until(expiry)))

		outcome, renewed := d.maintain(lease)
		switch outcome {
		case dhcpStopped:
			return
		case dhcpRenewed:
			lease = renewed
		case dhcpExpired:
			lease = nil
		}
	}
}

// maintain holds a bound lease through the RFC 2131 ladder: sleep until T1,
// then retry a unicast RENEW against the leasing server; from T2 fall back to a
// broadcast REBIND that any server on the segment may answer; only at expiry
// give the address up. The point is that losing the DHCP server for a few
// seconds must not cost the BMC its management address — the previous code
// dropped the lease on the first failed renew and took whatever DISCOVER
// offered next, which could silently move the IP.
//
// Kicks from the link monitor are serviced throughout, so an address wiped by a
// link flap is re-applied without waiting for the next renewal.
func (d *dhcpRunner) maintain(lease *nclient4.Lease) (dhcpOutcome, *nclient4.Lease) {
	t1, t2, expiry := leaseTimes(lease)

	for {
		now := time.Now()
		switch {
		case now.Before(t1):
			stopped, reacquire := d.sleepUntil(t1, lease)
			if stopped {
				return dhcpStopped, nil
			}
			if reacquire {
				return dhcpExpired, nil
			}

		case now.Before(expiry):
			rebinding := !now.Before(t2)
			fresh, err := d.extend(lease, rebinding)
			if err == nil {
				return dhcpRenewed, fresh
			}
			select {
			case <-d.done:
				return dhcpStopped, nil
			default:
			}
			// A NAK is authoritative: the binding is gone, so stop using the
			// address instead of holding it until expiry.
			var nak *nclient4.ErrNak
			if errors.As(err, &nak) {
				d.log.Warn("network: dhcp server rejected the lease; reacquiring", slog.String("iface", d.iface))
				return dhcpExpired, nil
			}
			d.log.Warn("network: dhcp lease extension failed; retrying",
				slog.String("iface", d.iface), slog.String("phase", phaseName(rebinding)),
				slog.Any("err", err), slog.Duration("expiresIn", until(expiry)))
			// Pace from now, not from the top of the iteration: a failed
			// exchange can itself burn dhcpOverallTimeout, and pacing off the
			// stale timestamp would leave no gap at all between attempts —
			// continuous DHCP traffic from T1 all the way to expiry. Clamped so
			// we never sleep past the expiry check.
			deadline := time.Now().Add(dhcpExtendRetry)
			if deadline.After(expiry) {
				deadline = expiry
			}
			stopped, reacquire := d.sleepUntil(deadline, lease)
			if stopped {
				return dhcpStopped, nil
			}
			if reacquire {
				return dhcpExpired, nil
			}

		default:
			d.log.Warn("network: dhcp lease expired; reacquiring", slog.String("iface", d.iface))
			return dhcpExpired, nil
		}
	}
}

// sleepUntil waits for a deadline while staying responsive to shutdown and to
// link-monitor kicks. On a kick it re-verifies the lease is still programmed (a
// link flap or netdev re-creation wipes it) and re-applies it; either way the
// caller's own loop re-reads the clock on its next iteration, so a kick needs
// no separate signal back to the caller.
func (d *dhcpRunner) sleepUntil(deadline time.Time, lease *nclient4.Lease) (stopped, reacquire bool) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-d.done:
		return true, false
	case <-timer.C:
		return false, false
	case <-d.reacquire:
		return false, true
	case <-d.kick:
		if !leaseApplied(d.iface, lease) {
			d.log.Warn("network: dhcp address lost; re-applying lease", slog.String("iface", d.iface))
			if err := d.applyLease(lease); err != nil {
				d.log.Warn("network: dhcp re-apply failed", slog.String("iface", d.iface), slog.Any("err", err))
			}
		}
		return false, false
	}
}

// waitBackoff paces a retry while no lease is held, returning early when the
// link comes back so a bring-up is never stuck behind a long backoff.
func (d *dhcpRunner) waitBackoff(dur time.Duration) (stopped bool) {
	timer := time.NewTimer(dur)
	defer timer.Stop()
	select {
	case <-d.done:
		return true
	case <-d.reacquire:
		return false
	case <-timer.C:
		return false
	}
}

func phaseName(rebinding bool) string {
	if rebinding {
		return "rebind"
	}
	return "renew"
}

// until renders a deadline as a whole-second duration for logging; already-past
// deadlines read as 0s rather than a negative.
func until(t time.Time) time.Duration {
	return max(time.Until(t).Truncate(time.Second), 0)
}

// leaseTimes derives the RFC 2131 T1 (renew), T2 (rebind) and expiry instants.
// Servers may state T1/T2 explicitly (options 58/59); when they do not, the
// spec's 0.5 and 0.875 fractions apply.
//
// The result is forced strictly increasing. Two things can otherwise break the
// ordering: a server is free to send an option 58/59 pair that does not make
// sense, and the dhcpMinRenew floor on T1 can push it past a very short lease.
// Pushing the later instants out rather than pulling T1 in is deliberate — the
// degenerate alternative is an expiry that precedes T1, where the runner would
// never attempt a renewal and would instead expire and re-DISCOVER in a loop.
func leaseTimes(lease *nclient4.Lease) (t1, t2, expiry time.Time) {
	base := lease.CreationTime
	if base.IsZero() {
		base = time.Now()
	}
	life := lease.ACK.IPAddressLeaseTime(dhcpFallbackLease)

	d1 := max(lease.ACK.IPAddressRenewalTime(life/2), dhcpMinRenew)
	d2 := lease.ACK.IPAddressRebindingTime(life * 7 / 8)
	if d2 <= d1 {
		d2 = d1 + dhcpExtendRetry
	}
	if life <= d2 {
		life = d2 + dhcpExtendRetry
	}
	return base.Add(d1), base.Add(d2), base.Add(life)
}

// ctxOrBackground defaults to context.Background() for a runner built
// without one (a test constructing a bare dhcpRunner literal); every
// production construction (see superviseEth0) sets ctx to the owning
// manager's.
func (d *dhcpRunner) ctxOrBackground() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

func (d *dhcpRunner) signalAttempt() {
	if d.onAttempt != nil {
		d.onAttempt() // sync.Once on the manager side; safe to call repeatedly
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
// manager shuts down, so Stop/Restart never block behind an in-flight
// exchange. Rooting directly at the runner's own ctx (the manager's, wired
// through at construction) is what makes the early cancellation immediate
// without a bridge goroutine forwarding a done-channel close into a
// context.Background()-rooted one.
func exchangeContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// obtain gets a lease from scratch. Given an address remembered from a previous
// run it first tries INIT-REBOOT (RFC 2131 4.4.2) — a broadcast REQUEST naming
// that address — which keeps the BMC's management IP stable across reboots and
// skips a DISCOVER round trip. Anything other than an ACK falls through to the
// full DISCOVER/REQUEST, so a stale remembered address costs one exchange.
func (d *dhcpRunner) obtain(remembered net.IP) (*nclient4.Lease, error) {
	if remembered != nil {
		lease, err := d.initReboot(remembered)
		if err == nil {
			d.log.Info("network: dhcp reclaimed remembered address", slog.String("iface", d.iface), slog.Any("addr", remembered))
			return lease, nil
		}
		// Drop it now rather than leaving it to be overwritten by the next
		// successful bind. The file is only rewritten on success, so a
		// remembered address that no server will confirm -- typically one from
		// a different network -- would otherwise cost this attempt on every
		// single start until DHCP eventually succeeds.
		d.log.Info("network: dhcp could not reclaim remembered address; forgetting it and running discover",
			slog.String("iface", d.iface), slog.Any("addr", remembered), slog.Any("err", err))
		d.forgetRememberedAddr()
	}

	ctx, c, cancel, err := d.exchange(dhcpOverallTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	defer cancel()
	return c.Request(ctx, d.options(c)...)
}

// initReboot sends the INIT-REBOOT REQUEST: no server identifier, the wanted
// address in option 50, broadcast so any server holding the binding can answer.
func (d *dhcpRunner) initReboot(want net.IP) (*nclient4.Lease, error) {
	ctx, c, cancel, err := d.exchange(dhcpRebootTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	defer cancel()

	req, err := dhcpv4.New(dhcpv4.PrependModifiers(d.options(c),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithHwAddr(c.InterfaceAddr()),
		dhcpv4.WithBroadcast(true),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(nclient4.MaxMessageSize)),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(want)),
		dhcpv4.WithRequestedOptions(
			dhcpv4.OptionSubnetMask,
			dhcpv4.OptionRouter,
			dhcpv4.OptionDomainName,
			dhcpv4.OptionDomainNameServer,
			dhcpv4.OptionNTPServers,
		),
	)...)
	if err != nil {
		return nil, fmt.Errorf("build init-reboot request: %w", err)
	}
	return finishExchange(ctx, c, req)
}

// extend asks for more time on the lease we already hold: a unicast RENEW to
// the leasing server, or past T2 a broadcast REBIND any server may answer.
func (d *dhcpRunner) extend(lease *nclient4.Lease, rebinding bool) (*nclient4.Lease, error) {
	ctx, c, cancel, err := d.exchange(dhcpOverallTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	defer cancel()

	if !rebinding {
		return c.Renew(ctx, lease, d.options(c)...)
	}

	// nclient4 has no rebind, but a REBIND is just the RENEW packet sent to the
	// broadcast address with any server allowed to answer. Modifiers passed here
	// are applied after the builder's own, so WithBroadcast(true) wins over the
	// unicast default.
	req, err := dhcpv4.NewRenewFromAck(lease.ACK, append(d.options(c),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(nclient4.MaxMessageSize)),
		dhcpv4.WithBroadcast(true))...)
	if err != nil {
		return nil, fmt.Errorf("build rebind: %w", err)
	}
	return finishExchange(ctx, c, req)
}

// finishExchange sends a hand-built REQUEST and turns the reply into a lease,
// mapping a NAK onto the same ErrNak the library's own paths return so callers
// can treat "the server says no" uniformly.
//
// The ACK is stored as the lease's Offer as well. That field is not decoration:
// nclient4.Renew dereferences lease.Offer to filter replies by server
// identifier, so a lease carrying a nil Offer — which an INIT-REBOOT or REBIND
// has no offer to fill — would panic on its first renewal. The ACK carries the
// same server identifier, and after a rebind it names the server that actually
// answered rather than the one we started with.
func finishExchange(ctx context.Context, c *nclient4.Client, req *dhcpv4.DHCPv4) (*nclient4.Lease, error) {
	ack, err := c.SendAndRead(ctx, nclient4.DefaultServers, req,
		nclient4.IsMessageType(dhcpv4.MessageTypeAck, dhcpv4.MessageTypeNak))
	if err != nil {
		return nil, err
	}
	if ack.MessageType() == dhcpv4.MessageTypeNak {
		return nil, &nclient4.ErrNak{Offer: ack, Nak: ack}
	}
	return &nclient4.Lease{Offer: ack, ACK: ack, CreationTime: time.Now()}, nil
}

// exchange opens a client bound to the interface together with the context that
// bounds the transaction. Callers must Close the client and call cancel.
func (d *dhcpRunner) exchange(timeout time.Duration) (context.Context, *nclient4.Client, context.CancelFunc, error) {
	c, err := nclient4.New(d.iface, nclient4.WithTimeout(min(dhcpExchangeTimeout, timeout)))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dhcp client: %w", err)
	}
	ctx, cancel := exchangeContext(d.ctxOrBackground(), timeout)
	return ctx, c, cancel, nil
}

// options are the modifiers added to every outgoing message. The hostname
// (option 12) is what makes the BMC show up under its own name in a server's
// lease table instead of as an anonymous MAC — without it UniFi and friends
// display nothing. The client identifier (option 61, RFC 2132 9.14: hardware
// type 1 followed by the MAC) gives the server a stable key for the binding.
func (d *dhcpRunner) options(c *nclient4.Client) []dhcpv4.Modifier {
	var mods []dhcpv4.Modifier
	if name, err := os.Hostname(); err == nil {
		if name = strings.TrimSpace(name); name != "" && name != "(none)" {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(name)))
		}
	}
	if hw := c.InterfaceAddr(); len(hw) > 0 {
		mods = append(mods, dhcpv4.WithOption(
			dhcpv4.OptClientIdentifier(append([]byte{0x01}, hw...))))
	}
	return mods
}

// ---- remembered address ----------------------------------------------------

func rememberedAddrPath(iface string) string {
	return filepath.Join(dhcpLeaseDir, fmt.Sprintf("dhcp-%s.addr", iface))
}

// rememberAddr records the bound address so the next run can ask for it back.
// Best-effort: the data partition may not be mounted yet, and losing the hint
// only costs a DISCOVER.
func (d *dhcpRunner) rememberAddr(ip net.IP) {
	if ip == nil || ip.IsUnspecified() {
		return
	}
	path := rememberedAddrPath(d.iface)
	if prev, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(prev)) == ip.String() {
		return // unchanged; do not rewrite on every renewal
	}
	if err := os.WriteFile(path, []byte(ip.String()+"\n"), 0o600); err != nil {
		d.log.Debug("network: dhcp remember address failed", slog.String("iface", d.iface), slog.Any("addr", ip), slog.Any("err", err))
	}
}

// forgetRememberedAddr drops the hint so a stale address cannot be retried on
// the next start. Best-effort: the worst case is one more failed reclaim.
func (d *dhcpRunner) forgetRememberedAddr() {
	if err := os.Remove(rememberedAddrPath(d.iface)); err != nil && !os.IsNotExist(err) {
		d.log.Debug("network: dhcp forget remembered address failed", slog.String("iface", d.iface), slog.Any("err", err))
	}
}

func loadRememberedAddr(iface string) net.IP {
	data, err := os.ReadFile(rememberedAddrPath(iface))
	if err != nil {
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(string(data)))
	if ip == nil {
		return nil
	}
	return ip.To4()
}

// dhcpNTP holds the NTP servers (option 42) from the most recent lease, kept
// for the timesync service. The last known set survives a lease loss — stale
// servers are still better than none while the link recovers.
var dhcpNTP struct {
	mu      sync.Mutex
	servers []string
}

// DHCPNTPServers returns the NTP servers announced in the most recent DHCP
// lease, or nil when the server offered none (or eth0 is static).
func DHCPNTPServers() []string {
	dhcpNTP.mu.Lock()
	defer dhcpNTP.mu.Unlock()
	return slices.Clone(dhcpNTP.servers)
}

func recordNTPServers(ips []net.IP) {
	servers := make([]string, 0, len(ips))
	for _, ip := range ips {
		servers = append(servers, ip.String())
	}
	dhcpNTP.mu.Lock()
	if len(servers) > 0 {
		dhcpNTP.servers = servers
	}
	dhcpNTP.mu.Unlock()
}

// applyLease programs the interface from a DHCP ACK: link up, address, default
// route and resolv.conf, all via netlink.
func (d *dhcpRunner) applyLease(lease *nclient4.Lease) error {
	ipnet, gw, dns, err := leaseAddrs(lease.ACK)
	if err != nil {
		return err
	}
	recordNTPServers(lease.ACK.NTPServers())
	link, err := netlink.LinkByName(d.iface)
	if err != nil {
		return fmt.Errorf("link %s: %w", d.iface, err)
	}
	if err := ensureUp(link); err != nil {
		return err
	}
	if err := replaceAddr(link, &netlink.Addr{IPNet: ipnet}, d.log); err != nil {
		return err
	}
	if gw != nil {
		if err := replaceDefaultRoute(link, gw); err != nil {
			d.log.Warn("network: set default route failed", slog.Any("err", err))
		}
	}
	writeResolvConf(dns, d.log)
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
