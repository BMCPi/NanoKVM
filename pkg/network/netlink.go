package network

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The netlink primitives here mirror the calls jetkvm-community/kvm funnels
// through its NetlinkManager (pkg/nmlite/link) and usb.go: LinkByName, LinkSetUp,
// AddrReplace, RouteReplace. They are deliberately thin and idempotent so they
// can be re-run on every reconcile or after a USB re-enumeration.

// waitForLink polls for a named link to appear, returning it once present. The
// primary NIC exists at boot, but the USB gadget's usb0 netdev only registers
// after the ncm function is created and the UDC bound, which is
// asynchronous — hence the retry (JetKVM's usb.go does the same 20×500ms loop).
func waitForLink(name string, attempts int, interval time.Duration, done <-chan struct{}) (netlink.Link, error) {
	for i := 0; i < attempts; i++ {
		if link, err := netlink.LinkByName(name); err == nil {
			return link, nil
		}
		select {
		case <-done:
			return nil, fmt.Errorf("stopped waiting for %s", name)
		case <-time.After(interval):
		}
	}
	return nil, fmt.Errorf("link %s did not appear after %d attempts", name, attempts)
}

// ensureUp sets the link administratively up (no-op if already up).
func ensureUp(link netlink.Link) error {
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set %s up: %w", link.Attrs().Name, err)
	}
	return nil
}

// ensureLinkUp looks a link up by name and sets it administratively up.
func ensureLinkUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link %s: %w", name, err)
	}
	return ensureUp(link)
}

// isAdminUp reports whether the link is administratively up.
func isAdminUp(link netlink.Link) bool {
	return link.Attrs().Flags&net.FlagUp != 0
}

// hasCarrier reports whether the link has a usable physical link, as opposed to
// merely being administratively up. IFF_RUNNING is the authoritative bit and
// stays correct on drivers that leave operstate at OperUnknown; operstate is
// consulted as well so either source can report the carrier.
func hasCarrier(attrs *netlink.LinkAttrs) bool {
	if attrs == nil {
		return false
	}
	return attrs.RawFlags&unix.IFF_RUNNING != 0 || attrs.OperState == netlink.OperUp
}

// hasAddr reports whether want is currently programmed on the link. Used by
// the reconcilers to keep the periodic health check silent when nothing is
// wrong.
func hasAddr(link netlink.Link, want *netlink.Addr) bool {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	wantOnes, wantBits := want.Mask.Size()
	for _, a := range addrs {
		ones, bits := a.Mask.Size()
		if a.IP.Equal(want.IP) && ones == wantOnes && bits == wantBits {
			return true
		}
	}
	return false
}

// bootMACOverride reads the operator MAC override from /boot/eth.mac (the job
// of the old if-pre-up.d/nanokvm-mac hook). Returns "" when the file is absent
// or does not hold a valid MAC. U-Boot writes the default eFUSE-derived
// address into the device tree unconditionally; this file is the only way for
// an operator to pin a different one.
func bootMACOverride() string {
	data, err := os.ReadFile("/boot/eth.mac")
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	mac := strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
	if _, err := net.ParseMAC(mac); err != nil {
		return ""
	}
	return mac
}

// hasMAC reports whether the link already carries mac, letting callers skip the
// down/set/up cycle that changing a hardware address requires. An unparsable
// value reports false so the caller falls through to setMAC and logs there.
func hasMAC(link netlink.Link, mac string) bool {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return false
	}
	return bytes.Equal(link.Attrs().HardwareAddr, hw)
}

// setMAC pins the link's hardware address. Must be applied while the link is
// down; callers set the MAC before ensureUp.
func setMAC(link netlink.Link, mac string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("parse mac %q: %w", mac, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, hw); err != nil {
		return fmt.Errorf("set %s mac: %w", link.Attrs().Name, err)
	}
	return nil
}

// replaceAddr idempotently makes want the interface's sole global IPv4 address:
// it AddrReplaces want (updating in place if the IP already matches) and removes
// any other global IPv4 address left over from a previous lease/config. IPv4
// link-local (169.254/16) addresses are preserved so the RHI's own address on a
// shared link is never torn down. Mirrors JetKVM's single-global-address model.
func replaceAddr(link netlink.Link, want *netlink.Addr) error {
	existing, _ := netlink.AddrList(link, netlink.FAMILY_V4)
	if err := netlink.AddrReplace(link, want); err != nil {
		return fmt.Errorf("addr replace %s on %s: %w", want, link.Attrs().Name, err)
	}
	for i := range existing {
		a := existing[i]
		if a.IP.Equal(want.IP) || a.IP.IsLinkLocalUnicast() {
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			slog.Warn("network: remove stale addr failed", slog.Any("addr", a.IPNet), slog.String("iface", link.Attrs().Name), slog.Any("err", err))
		}
	}
	return nil
}

// replaceDefaultRoute installs (or updates) a default route via gw on link.
func replaceDefaultRoute(link netlink.Link, gw net.IP) error {
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       nil, // default
		Gw:        gw,
		Scope:     netlink.SCOPE_UNIVERSE,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("default route via %s on %s: %w", gw, link.Attrs().Name, err)
	}
	return nil
}

// writeResolvConf overwrites /etc/resolv.conf with the given nameservers,
// matching what the old S30eth script wrote on a static/DHCP lease.
func writeResolvConf(servers []net.IP) {
	if len(servers) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("# generated by nanokvm network service\n")
	for _, ip := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", ip.String())
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644); err != nil { //nolint:gosec // G306: system resolver config -- every process doing name resolution (busybox wget/ping, the Go runtime's own resolver) reads this well-known path, so it must stay world-readable
		slog.Warn("network: write /etc/resolv.conf failed", slog.Any("err", err))
	}
}
