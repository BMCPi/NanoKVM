package network

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// applyRHIIsolation pins the Redfish-Host-Interface isolation knobs for the
// gadget NIC, replacing the build's ifupdown if-up.d hook (nanokvm-usb0-up).
// The BMC must never bridge or route between the host link and the management
// LAN, and the managed host must not be able to steer BMC routing:
//
//   - forwarding off on the link (v4+v6), plus an nftables guard that keeps it
//     out of the forward path even if ip_forward is later enabled globally
//     (e.g. for WireGuard).
//   - accept_ra off: no router advertisements from the host.
//   - ARP-flux containment so the link-local address never leaks onto the
//     management LAN via eth0, plus per-link anti-spoof/redirect hardening.
//
// There is no procps/sysctl init on this image, so the knobs are written
// straight into /proc — idempotent, re-applied on every RHI (re)configure,
// which is also exactly the right moment: the netdev exists and is up.
func applyRHIIsolation(iface string) {
	set := func(path, val string) {
		// Mode is ignored for existing /proc entries; a missing entry (e.g.
		// ipv6 disabled) is fine.
		if err := os.WriteFile(path, []byte(val), 0o600); err != nil && !os.IsNotExist(err) {
			slog.Debug("network: sysctl write failed", slog.String("path", path), slog.Any("err", err))
		}
	}

	v4 := "/proc/sys/net/ipv4/conf/" + iface
	set(v4+"/forwarding", "0")
	// Per-link hardening: reject source-spoofed packets from the host (strict
	// rp_filter), and let neither end steer routing via ICMP redirects.
	set(v4+"/rp_filter", "1")
	set(v4+"/accept_redirects", "0")
	set(v4+"/send_redirects", "0")

	v6 := "/proc/sys/net/ipv6/conf/" + iface
	set(v6+"/forwarding", "0")
	set(v6+"/accept_ra", "0")

	// ARP-flux containment. The BMC is multi-homed (eth0 -> LAN, usb0 -> host),
	// so with the default arp_ignore=0 it will answer an ARP request for the
	// RHI's 169.254 address that arrives on *eth0*, leaking the host-interface
	// address onto the management LAN. The kernel evaluates arp_{ignore,announce}
	// as max(conf/all, conf/<arrival-if>), and the arrival interface is eth0,
	// so the fix must live on conf/all:
	//   arp_ignore=1   -> reply only when the target IP is on the arrival iface.
	//   arp_announce=2 -> always source ARP with the best address for the
	//                     target, so 169.254 is never advertised out eth0.
	// Both are standard hardening values and safe for a single-homed eth0.
	set("/proc/sys/net/ipv4/conf/all/arp_ignore", "1")
	set("/proc/sys/net/ipv4/conf/all/arp_announce", "2")

	ensureNFTGuard(iface)
}

// ensureNFTGuard installs an nftables table dropping the RHI link from the
// forward path in both directions. Declaring the table first makes the delete
// safe on first run, so re-running replaces rather than duplicates the rules.
// Degrades gracefully when the nft binary is absent.
func ensureNFTGuard(iface string) {
	nft, err := exec.LookPath("nft")
	if err != nil {
		return
	}
	script := fmt.Sprintf(`table inet nanokvm_usb0
delete table inet nanokvm_usb0
table inet nanokvm_usb0 {
	chain forward {
		type filter hook forward priority -10; policy accept;
		iifname %q counter drop
		oifname %q counter drop
	}
}
`, iface, iface)
	cmd := exec.CommandContext(context.Background(), nft, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("network: nft guard failed", slog.Any("err", err), slog.String("output", strings.TrimSpace(string(out))))
	}
}
