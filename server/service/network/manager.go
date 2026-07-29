// Package network configures the BMC's host-facing interfaces directly via
// netlink, replacing the shell ip/udhcpc/ifupdown setup. It owns two links,
// following the split in jetkvm-community/kvm:
//
//   - eth0, the primary wired uplink: brought up and addressed either statically
//     (from config) or by an in-process DHCPv4 client (see dhcp.go).
//   - usb0, the USB Redfish-Host-Interface link the gadget exposes: a static
//     IPv4 link-local address (169.254.10.1/16), re-asserted whenever the netdev
//     reappears after a USB re-enumeration.
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

// Manager owns the lifecycle of the interface-configuration goroutines. Stop
// closes the shared done channel, unwinding the DHCP loop, the RHI supervisor
// and the link monitor.
type Manager struct {
	cfg config.Network

	mu   sync.Mutex
	done chan struct{}
}

// Start reads config and, when enabled, begins configuring eth0 and the RHI
// link in the background. It returns immediately; interface bring-up (which may
// wait for a netdev to appear) happens in goroutines. Returns (nil, nil) when
// disabled in config. Follows the repo's Start()-returning-a-handle pattern
// (mdns.Start, ipmi.Start).
func Start() (*Manager, error) {
	cfg := config.GetInstance().Network
	if !cfg.Enabled {
		log.Info("network: disabled by config; leaving interface setup to init scripts")
		return nil, nil
	}

	m := &Manager{cfg: cfg, done: make(chan struct{})}
	done := m.done

	if cfg.Eth0.Name != "" {
		go m.configureEth0(done)
	}
	if cfg.RHI.Interface != "" && cfg.RHI.Address != "" {
		go m.superviseRHI(done)
	}
	return m, nil
}

// Stop tears down the background goroutines. Idempotent.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done != nil {
		close(m.done)
		m.done = nil
	}
}

// ---- eth0 ------------------------------------------------------------------

func (m *Manager) configureEth0(done <-chan struct{}) {
	ic := m.cfg.Eth0

	link, err := waitForLink(ic.Name, 20, 500*time.Millisecond, done)
	if err != nil {
		log.Errorf("network: eth0: %v", err)
		return
	}

	if ic.MAC != "" {
		// A hardware address can only be set while the link is down.
		_ = netlink.LinkSetDown(link)
		if err := setMAC(link, ic.MAC); err != nil {
			log.Warnf("network: eth0: %v", err)
		}
	}
	if err := ensureUp(link); err != nil {
		log.Errorf("network: eth0: %v", err)
		return
	}

	switch strings.ToLower(ic.Mode) {
	case "static":
		if err := m.applyStatic(link, ic); err != nil {
			log.Errorf("network: eth0 static config: %v", err)
		}
	default: // "dhcp" or unset
		(&dhcpRunner{iface: ic.Name, done: done}).run()
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
	// The usb0 netdev registers asynchronously after the gadget binds its UDC,
	// so wait for it (JetKVM's usb.go retries the same way).
	if link, err := waitForLink(m.cfg.RHI.Interface, 40, 500*time.Millisecond, done); err != nil {
		log.Warnf("network: RHI: %v", err)
	} else {
		m.configureRHI(link)
	}
	// Then re-assert the address whenever usb0 reappears (USB re-enumeration
	// after a UDC rebind recreates the netdev).
	m.monitorRHI(done)
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
}

func (m *Manager) monitorRHI(done <-chan struct{}) {
	ch := make(chan netlink.LinkUpdate)
	if err := netlink.LinkSubscribe(ch, done); err != nil {
		log.Warnf("network: link monitor unavailable; RHI will not auto-reassert: %v", err)
		return
	}
	for {
		select {
		case <-done:
			return
		case u, ok := <-ch:
			if !ok {
				return
			}
			if u.Link == nil || u.Link.Attrs() == nil || u.Link.Attrs().Name != m.cfg.RHI.Interface {
				continue
			}
			if link, err := netlink.LinkByName(m.cfg.RHI.Interface); err == nil {
				m.configureRHI(link)
			}
		}
	}
}
