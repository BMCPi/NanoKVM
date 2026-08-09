// Package network configures the BMC's host-facing interfaces directly via
// netlink, replacing the shell ip/udhcpc/ifupdown setup. It owns two links,
// following the split in jetkvm-community/kvm:
//
//   - eth0, the primary wired uplink: brought up and addressed either statically
//     (from config) or by an in-process DHCPv4 client (see dhcp.go).
//   - usb0, the USB Redfish-Host-Interface link the gadget exposes: a static
//     IPv4 link-local address (169.254.10.1/16), re-asserted whenever the netdev
//     reappears after a USB re-enumeration.
//
// The package supervises both links for their whole lifetime: a netlink link
// monitor plus a periodic reconcile re-applies configuration whenever an
// interface breaks (netdev recreated, admin-down, address lost), and Restart
// re-reads config so settings changed through the app take effect immediately.
package network

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/pi-bmc/nanokvm-app/server/config"
)

const (
	// ModeDHCP/ModeStatic are the two eth0 addressing modes.
	ModeDHCP   = "dhcp"
	ModeStatic = "static"

	// reconcileInterval is the fallback cadence at which both interfaces are
	// re-verified even when no link event arrives (covers a missed or
	// unavailable netlink subscription).
	reconcileInterval = 30 * time.Second
	// supRetryFloor/supRetryCap bound the backoff between failed eth0 bring-up
	// attempts (link never appearing, LinkSetUp errors, static apply errors).
	supRetryFloor = 5 * time.Second
	supRetryCap   = 60 * time.Second
)

// rhiEth0Wait caps how long the RHI defers to eth0. The uplink is configured
// first so the management interface owns the default route and resolv.conf
// before anything else touches the network, but the RHI is the out-of-band path
// to the managed host -- the one that still works when the uplink does not --
// so it must never be held hostage to eth0. Past this cap it comes up
// regardless. A variable only so tests need not wait it out.
var rhiEth0Wait = 30 * time.Second

// Manager owns the lifecycle of the interface-configuration goroutines. Stop
// closes the shared done channel, unwinding the DHCP loop, the supervisors and
// the link monitor, and waits for them to exit so a Restart never races a
// previous incarnation.
type Manager struct {
	cfg config.Network

	mu   sync.Mutex
	done chan struct{}
	wg   sync.WaitGroup

	// eth0Ready/rhiReady close once the first configuration attempt for that
	// link has completed — successfully or not. WaitReady gates server startup
	// on the attempt, not on the network being healthy, so an unplugged cable
	// or absent DHCP server can never wedge boot.
	eth0Ready chan struct{}
	eth0Once  sync.Once
	rhiReady  chan struct{}
	rhiOnce   sync.Once

	// dhcpKick wakes the DHCP runner to re-verify its lease is still
	// programmed (fed by link events and the reconcile ticker).
	dhcpKick chan struct{}

	// dhcpReacquire tells the DHCP runner to throw its lease away and start a
	// fresh DISCOVER. Raised on the edge where eth0 gains carrier, so every
	// bring-up -- boot, cable replug, switch reboot, link recovery -- goes out
	// and asks again rather than assuming the address it held still belongs to
	// it. A carrier bounce does not remove addresses from a netdev, so without
	// this the runner would look at a still-programmed address, conclude
	// nothing was lost, and silently keep a lease that may be from a different
	// network entirely.
	dhcpReacquire chan struct{}

	// eth0Carrier is the last observed carrier state, so the edge can be
	// detected rather than re-triggering on every event while it stays up.
	// Guarded by mu.
	eth0Carrier bool

	// rhiDHCP is the single-lease DHCP server for the USB host link; restarted
	// on every RHI (re)configure so it survives netdev re-creation. Guarded by
	// mu.
	rhiDHCP *rhiDHCPServer
}

var (
	activeMu sync.Mutex
	active   *Manager
)

// Start reads config and, when enabled, begins configuring eth0 and the RHI
// link in the background. It returns immediately; interface bring-up (which may
// wait for a netdev to appear) happens in goroutines. Callers that need the
// initial configuration applied first should follow with WaitReady.
func Start() {
	activeMu.Lock()
	defer activeMu.Unlock()
	startLocked()
}

// Stop tears down the active manager and waits for its goroutines. Idempotent.
func Stop() {
	activeMu.Lock()
	m := active
	active = nil
	activeMu.Unlock()
	if m != nil {
		m.stop()
	}
}

// Restart tears down the active manager (if any) and starts a fresh one from
// the current config. Called by the settings handlers after a network config
// change so the new addressing is applied without a process restart.
func Restart() {
	activeMu.Lock()
	defer activeMu.Unlock()
	if active != nil {
		active.stop()
		active = nil
	}
	startLocked()
}

// WaitReady blocks until the active manager's initial configuration pass has
// completed for every supervised link, or the timeout elapses. A no-op when
// networking is disabled.
func WaitReady(timeout time.Duration) {
	activeMu.Lock()
	m := active
	activeMu.Unlock()
	if m != nil {
		m.waitReady(timeout)
	}
}

func startLocked() {
	cfg := config.GetInstance().Network
	if !cfg.Enabled {
		log.Info("network: disabled by config; leaving interface setup to init scripts")
		return
	}

	m := &Manager{
		cfg:           cfg,
		done:          make(chan struct{}),
		eth0Ready:     make(chan struct{}),
		rhiReady:      make(chan struct{}),
		dhcpKick:      make(chan struct{}, 1),
		dhcpReacquire: make(chan struct{}, 1),
	}

	if cfg.Eth0.Name != "" {
		m.wg.Add(1)
		go m.superviseEth0(m.done)
	} else {
		m.signalEth0Ready()
	}
	if cfg.RHI.Interface != "" && cfg.RHI.Address != "" {
		m.wg.Add(1)
		go func() {
			m.awaitEth0(m.done)
			m.superviseRHI(m.done)
		}()
	} else {
		m.signalRHIReady()
	}
	m.wg.Add(1)
	go m.monitorLinks(m.done)

	active = m
}

func (m *Manager) stop() {
	m.mu.Lock()
	if m.done != nil {
		close(m.done)
		m.done = nil
	}
	m.mu.Unlock()
	m.wg.Wait()

	m.mu.Lock()
	if m.rhiDHCP != nil {
		m.rhiDHCP.stop()
		m.rhiDHCP = nil
	}
	m.mu.Unlock()
}

// awaitEth0 defers a dependent interface until eth0's first configuration
// attempt has completed, so the uplink is addressed -- and owns the default
// route and resolv.conf -- before anything else is brought up.
//
// It waits on the attempt, not on success: eth0Ready closes whether the attempt
// bound a lease or gave up, so an unplugged cable or absent DHCP server costs
// the wait once rather than forever. rhiEth0Wait is the backstop for the case
// where even that signal is slow, because the RHI is the access path that has
// to survive a broken uplink.
func (m *Manager) awaitEth0(done <-chan struct{}) {
	if m.cfg.Eth0.Name == "" {
		return
	}
	timer := time.NewTimer(rhiEth0Wait)
	defer timer.Stop()

	select {
	case <-m.eth0Ready:
	case <-done:
	case <-timer.C:
		log.Warnf("network: %s not configured after %s; bringing up %s anyway",
			m.cfg.Eth0.Name, rhiEth0Wait, m.cfg.RHI.Interface)
	}
}

func (m *Manager) signalEth0Ready() { m.eth0Once.Do(func() { close(m.eth0Ready) }) }
func (m *Manager) signalRHIReady()  { m.rhiOnce.Do(func() { close(m.rhiReady) }) }

func (m *Manager) waitReady(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for _, w := range []struct {
		name  string
		ready <-chan struct{}
	}{
		{"eth0", m.eth0Ready},
		{"rhi", m.rhiReady},
	} {
		select {
		case <-w.ready:
		case <-timer.C:
			log.Warnf("network: timed out waiting for initial %s configuration; continuing startup", w.name)
			return
		}
	}
	log.Info("network: initial interface configuration complete")
}

// ---- eth0 ------------------------------------------------------------------

// superviseEth0 brings the uplink up, retrying with backoff instead of giving
// up: a missing netdev, a failed LinkSetUp or a failed static apply are all
// transient on this hardware. Once configured, ongoing health is owned by the
// DHCP runner (dhcp mode) or the link monitor's reconcile (static mode).
func (m *Manager) superviseEth0(done <-chan struct{}) {
	defer m.wg.Done()
	ic := m.cfg.Eth0
	backoff := supRetryFloor

	for {
		link, err := waitForLink(ic.Name, 20, 500*time.Millisecond, done)
		if err != nil {
			log.Errorf("network: eth0: %v (retry in %s)", err, backoff)
			m.signalEth0Ready()
			if sleepOrDone(done, backoff) {
				return
			}
			backoff = growBackoff(backoff)
			continue
		}

		// Config MAC wins; otherwise honor the operator override file on the
		// boot partition (the job the old if-pre-up.d/nanokvm-mac hook did).
		// The default stable MAC needs neither: U-Boot derives it from the
		// eFUSE and writes it into the device tree, which stmmac consumes.
		mac := ic.MAC
		if mac == "" {
			mac = bootMACOverride()
		}
		if mac != "" && !hasMAC(link, mac) {
			// A hardware address can only be set while the link is down, so this
			// is the one place in the package that can flap a healthy link.
			// Skipping it when the address already matches matters because
			// Restart() re-runs this supervisor: without the guard, saving any
			// unrelated network setting would bounce eth0 (and with it the DHCP
			// lease and every open session) for no reason.
			_ = netlink.LinkSetDown(link)
			if err := setMAC(link, mac); err != nil {
				log.Warnf("network: eth0: %v", err)
			}
		}
		if err := ensureUp(link); err != nil {
			log.Errorf("network: eth0: %v (retry in %s)", err, backoff)
			m.signalEth0Ready()
			if sleepOrDone(done, backoff) {
				return
			}
			backoff = growBackoff(backoff)
			continue
		}

		switch strings.ToLower(ic.Mode) {
		case ModeStatic:
			if err := m.applyStatic(link, ic); err != nil {
				log.Errorf("network: eth0 static config: %v (retry in %s)", err, backoff)
				m.signalEth0Ready()
				if sleepOrDone(done, backoff) {
					return
				}
				backoff = growBackoff(backoff)
				continue
			}
			m.signalEth0Ready()
			// The link monitor re-applies the static config if the address or
			// link state is later lost; nothing more to do here.
			return
		default: // "dhcp" or unset
			// The runner is self-healing (retries acquisition, re-applies a
			// lost lease on kicks, reacquires on failure) and only returns on
			// shutdown.
			(&dhcpRunner{
				iface:     ic.Name,
				done:      done,
				kick:      m.dhcpKick,
				reacquire: m.dhcpReacquire,
				onAttempt: m.signalEth0Ready,
			}).run()
			return
		}
	}
}

func (m *Manager) applyStatic(link netlink.Link, ic config.InterfaceConfig) error {
	if ic.Address == "" {
		return fmt.Errorf("static mode but no address configured")
	}
	addr, err := netlink.ParseAddr(ic.Address)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", ic.Address, err)
	}
	if err := replaceAddr(link, addr); err != nil {
		return err
	}
	if ic.Gateway != "" {
		gw := net.ParseIP(ic.Gateway)
		if gw == nil {
			log.Warnf("network: eth0: invalid gateway %q", ic.Gateway)
		} else if err := replaceDefaultRoute(link, gw); err != nil {
			log.Warnf("network: eth0: %v", err)
		}
	}
	writeResolvConf(parseDNS(ic.DNS))
	log.Infof("network: eth0 static %s gw=%s", ic.Address, ic.Gateway)
	return nil
}

// reconcileEth0Static re-applies the static config when the link is down or
// the address has gone missing (netdev recreated, address flushed). Checks
// health first so the periodic tick is silent on a healthy link.
func (m *Manager) reconcileEth0Static() {
	ic := m.cfg.Eth0
	link, err := netlink.LinkByName(ic.Name)
	if err != nil {
		// Netdev currently absent; the NEWLINK event on reappearance (or the
		// next tick) retries.
		return
	}
	addr, err := netlink.ParseAddr(ic.Address)
	if err != nil {
		return // already logged by the initial apply
	}
	if isAdminUp(link) && hasAddr(link, addr) {
		return
	}
	log.Warnf("network: eth0 static config lost; re-applying")
	if err := ensureUp(link); err != nil {
		log.Warnf("network: eth0: %v", err)
		return
	}
	if err := m.applyStatic(link, ic); err != nil {
		log.Warnf("network: eth0 static re-apply: %v", err)
	}
}

func parseDNS(list []string) []net.IP {
	out := make([]net.IP, 0, len(list))
	for _, s := range list {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		} else {
			log.Warnf("network: ignoring invalid dns server %q", s)
		}
	}
	return out
}

// ---- usb0 / RHI ------------------------------------------------------------

func (m *Manager) superviseRHI(done <-chan struct{}) {
	defer m.wg.Done()
	// The usb0 netdev registers asynchronously after the gadget binds its UDC,
	// so wait for it (JetKVM's usb.go retries the same way). Ongoing
	// re-assertion after USB re-enumeration is owned by the link monitor.
	if link, err := waitForLink(m.cfg.RHI.Interface, 40, 500*time.Millisecond, done); err != nil {
		log.Warnf("network: RHI: %v", err)
	} else {
		m.configureRHI(link)
	}
	m.signalRHIReady()
}

func (m *Manager) configureRHI(link netlink.Link) {
	if err := ensureUp(link); err != nil {
		log.Warnf("network: RHI: %v", err)
		return
	}
	addr, err := netlink.ParseAddr(m.cfg.RHI.Address)
	if err != nil {
		log.Errorf("network: RHI address %q: %v", m.cfg.RHI.Address, err)
		return
	}
	// AddrReplace is idempotent — safe to re-run on every link event. This is
	// the RHI's own link-local address; we do not disturb any other address.
	if err := netlink.AddrReplace(link, addr); err != nil {
		log.Warnf("network: RHI addr %s on %s: %v", m.cfg.RHI.Address, m.cfg.RHI.Interface, err)
		return
	}
	log.Infof("network: RHI %s = %s", m.cfg.RHI.Interface, m.cfg.RHI.Address)

	// The rest of the RHI contract, formerly the build's ifupdown hooks:
	// isolation knobs + nft guard, and the single-lease DHCP server for the
	// host side. Both are idempotent and re-applied on every (re)configure —
	// a recreated netdev arrives with default sysctls and no listener.
	applyRHIIsolation(m.cfg.RHI.Interface)
	m.ensureRHIDHCP(addr)
}

// ensureRHIDHCP (re)starts the single-lease DHCP server on the RHI link. The
// old server is always torn down first: after a netdev re-creation its socket
// is bound to a dead interface index.
func (m *Manager) ensureRHIDHCP(addr *netlink.Addr) {
	lease := m.cfg.RHI.Lease
	if lease == "" {
		return
	}
	leaseIP := net.ParseIP(lease)
	if leaseIP == nil {
		log.Errorf("network: RHI lease %q: not an IP address", lease)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rhiDHCP != nil {
		m.rhiDHCP.stop()
		m.rhiDHCP = nil
	}
	srv, err := startRHIDHCP(m.cfg.RHI.Interface, addr.IP, leaseIP, addr.Mask)
	if err != nil {
		log.Warnf("network: RHI dhcp server: %v", err)
		return
	}
	m.rhiDHCP = srv
}

// reconcileRHI re-asserts the RHI address when the link is down or the address
// is missing (USB re-enumeration recreates the netdev). Health-checked first
// so the periodic tick does not spam the log.
func (m *Manager) reconcileRHI() {
	link, err := netlink.LinkByName(m.cfg.RHI.Interface)
	if err != nil {
		return // gadget NIC currently absent (e.g. ethernet function off)
	}
	addr, err := netlink.ParseAddr(m.cfg.RHI.Address)
	if err != nil {
		return // already logged by the initial configure
	}
	if isAdminUp(link) && hasAddr(link, addr) {
		return
	}
	m.configureRHI(link)
}

// ---- link monitor / reconcile ----------------------------------------------

// monitorLinks watches netlink link events and runs a periodic reconcile,
// re-configuring whichever supervised interface has drifted (netdev recreated,
// admin-down, address lost). The ticker also covers the case where the event
// subscription is unavailable or an event is missed.
func (m *Manager) monitorLinks(done <-chan struct{}) {
	defer m.wg.Done()

	events := make(chan netlink.LinkUpdate, 16)
	if err := netlink.LinkSubscribe(events, done); err != nil {
		log.Warnf("network: link monitor unavailable (%v); relying on periodic reconcile", err)
		events = nil
	}

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case u, ok := <-events:
			if !ok {
				events = nil // nil channel: select never picks this case again
				continue
			}
			if u.Link == nil || u.Attrs() == nil {
				continue
			}
			if u.Attrs().Name == m.cfg.Eth0.Name {
				m.noteEth0Carrier(hasCarrier(u.Attrs()))
			}
			m.handleLinkEvent(u.Attrs().Name)
		case <-ticker.C:
			m.reconcile()
		}
	}
}

// noteEth0Carrier records the carrier state and, on the transition to carrier
// present, asks the DHCP runner to reacquire. Only the rising edge signals, so
// a link that stays up does not restart DHCP on every unrelated netlink event.
func (m *Manager) noteEth0Carrier(up bool) {
	m.mu.Lock()
	rose := up && !m.eth0Carrier
	m.eth0Carrier = up
	m.mu.Unlock()

	if !rose || !dhcpMode(m.cfg.Eth0.Mode) {
		return
	}
	log.Infof("network: %s carrier up; reacquiring a lease", m.cfg.Eth0.Name)
	select {
	case m.dhcpReacquire <- struct{}{}:
	default: // one pending reacquire is as good as several
	}
}

func (m *Manager) handleLinkEvent(name string) {
	if name == m.cfg.RHI.Interface {
		m.reconcileRHI()
	}
	if name == m.cfg.Eth0.Name {
		m.reconcileEth0()
	}
}

func (m *Manager) reconcile() {
	if m.cfg.RHI.Interface != "" && m.cfg.RHI.Address != "" {
		m.reconcileRHI()
	}
	if m.cfg.Eth0.Name != "" {
		// Re-read the carrier from the kernel as well: this is the backstop for
		// a missed or dropped netlink event, and it is what makes the rising
		// edge reliable rather than best-effort.
		if link, err := netlink.LinkByName(m.cfg.Eth0.Name); err == nil {
			m.noteEth0Carrier(hasCarrier(link.Attrs()))
		} else {
			m.noteEth0Carrier(false)
		}
		m.reconcileEth0()
	}
}

func (m *Manager) reconcileEth0() {
	switch strings.ToLower(m.cfg.Eth0.Mode) {
	case ModeStatic:
		m.reconcileEth0Static()
	default:
		// The DHCP runner owns the interface; wake it to verify its lease is
		// still programmed and re-apply/reacquire if not.
		select {
		case m.dhcpKick <- struct{}{}:
		default:
		}
	}
}

// dhcpMode reports whether an interface is DHCP-addressed. Anything not
// explicitly static is, which mirrors superviseEth0's switch so the two can
// never disagree about which runner owns the link.
func dhcpMode(mode string) bool {
	return !strings.EqualFold(mode, ModeStatic)
}

func growBackoff(d time.Duration) time.Duration {
	return min(d*2, supRetryCap)
}
