// Package mdns is a multicast-DNS hostname responder built on pion/mdns,
// replacing avahi-daemon. Following the JetKVM internal/mdns pattern it does
// hostname resolution only — it answers multicast A/AAAA queries for
// <hostname>.local and publishes no service (_http._tcp) or TXT records, which
// matches what avahi advertised on this image.
//
// The responder is scoped to a single interface (eth0 by default) so the
// point-to-point USB host link (usb0, 169.254.10.1) never receives duplicate
// records for the managed host — the same scoping the old avahi bbappend
// enforced with allow-interfaces=eth0.
//
// pion caches the interface's addresses (and skips down interfaces) at start,
// so a background watcher restarts the responder when the hostname or the
// interface's addresses change — e.g. once eth0 obtains a DHCP lease. This
// mirrors JetKVM restarting mDNS on network-state changes.
package mdns

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	pionmdns "github.com/pion/mdns/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

// watchInterval is how often the watcher re-checks the hostname / interface
// addresses and restarts the responder if they changed.
const watchInterval = 15 * time.Second

// Responder is a running mDNS hostname responder.
type Responder struct {
	mu   sync.Mutex
	conn *pionmdns.Conn

	ifaceName string
	ipv4      bool
	ipv6      bool
	hostname  string   // configured override, or "" to use the OS hostname
	names     []string // currently advertised names (e.g. "nanokvm.local")
	lastSig   string   // hostname + interface addresses at the last (re)start

	stopCh   chan struct{}
	stopOnce sync.Once
}

// The process-wide singleton, so the vm info endpoint can report the advertised
// name without threading the pointer through every service.
var (
	mu      sync.Mutex
	current *Responder
)

// Start builds and starts the responder from config, stores it as the process
// singleton, and launches the restart watcher. It returns (nil, nil) when mDNS
// is disabled. An initial bind failure is not fatal — the watcher retries (the
// target interface may not be up yet).
func Start() (*Responder, error) {
	cfg := config.GetInstance().MDNS
	if !cfg.Enabled {
		log.Info("mdns: disabled by config")
		return nil, nil
	}
	if !cfg.IPv4 && !cfg.IPv6 {
		log.Info("mdns: both ipv4 and ipv6 disabled; responder inactive")
		return nil, nil
	}

	r := &Responder{
		ifaceName: cfg.Interface,
		ipv4:      cfg.IPv4,
		ipv6:      cfg.IPv6,
		hostname:  cfg.Hostname,
		stopCh:    make(chan struct{}),
	}
	if err := r.start(); err != nil {
		log.Warnf("mdns: initial start failed (watcher will retry): %v", err)
	}
	go r.watch()

	mu.Lock()
	current = r
	mu.Unlock()
	return r, nil
}

// Advertised returns the name the running responder publishes (e.g.
// "nanokvm.local") and whether the responder is active. Used by the vm info
// endpoint in place of the old avahi PID-file probe.
func Advertised() (string, bool) {
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil {
		return "", false
	}
	return r.Name()
}

// Name returns the primary advertised name when the responder is running.
func (r *Responder) Name() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil || len(r.names) == 0 {
		return "", false
	}
	return r.names[0], true
}

// Stop tears down the responder and its watcher. Safe to call more than once.
func (r *Responder) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
	r.mu.Lock()
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	r.mu.Unlock()
}

func (r *Responder) start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked()
}

func (r *Responder) startLocked() error {
	// Close any existing responder first (this doubles as Restart).
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}

	names := localNames(r.resolveHostname())
	if len(names) == 0 {
		return fmt.Errorf("no hostname to advertise")
	}

	ifaces, err := r.interfaces()
	if err != nil {
		return err
	}

	var p4 *ipv4.PacketConn
	var p6 *ipv6.PacketConn
	if r.ipv4 {
		if addr, err := net.ResolveUDPAddr("udp4", pionmdns.DefaultAddressIPv4); err == nil {
			if l4, err := net.ListenUDP("udp4", addr); err != nil {
				log.Warnf("mdns: ipv4 listen failed: %v", err)
			} else {
				p4 = ipv4.NewPacketConn(l4)
			}
		}
	}
	if r.ipv6 {
		if addr, err := net.ResolveUDPAddr("udp6", pionmdns.DefaultAddressIPv6); err == nil {
			if l6, err := net.ListenUDP("udp6", addr); err != nil {
				log.Warnf("mdns: ipv6 listen failed: %v", err)
			} else {
				p6 = ipv6.NewPacketConn(l6)
			}
		}
	}
	if p4 == nil && p6 == nil {
		return fmt.Errorf("no multicast socket could be bound")
	}

	conn, err := pionmdns.Server(p4, p6, &pionmdns.Config{
		Name:       "nanokvm",
		LocalNames: names,
		Interfaces: ifaces,
	})
	if err != nil {
		if p4 != nil {
			_ = p4.Close()
		}
		if p6 != nil {
			_ = p6.Close()
		}
		return fmt.Errorf("start mdns server: %w", err)
	}

	r.conn = conn
	r.names = names
	r.lastSig = r.signature(ifaces)
	log.Infof("mdns: advertising %v on %s (ipv4=%v ipv6=%v)", names, r.ifaceDesc(), p4 != nil, p6 != nil)
	return nil
}

// watch restarts the responder whenever the hostname or the interface's
// addresses change (pion cached them at start), and retries while it is not yet
// running (e.g. eth0 still coming up).
func (r *Responder) watch() {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			ifaces, _ := r.interfaces()
			sig := r.signature(ifaces)

			r.mu.Lock()
			changed := sig != r.lastSig
			running := r.conn != nil
			r.mu.Unlock()

			if !changed && running {
				continue
			}
			if err := r.start(); err != nil {
				log.Debugf("mdns: restart on change failed (will retry): %v", err)
			} else {
				log.Debug("mdns: (re)started after network/hostname change")
			}
		}
	}
}

// interfaces resolves the configured interface name to a []net.Interface for
// pion. An empty name yields nil (pion then uses all non-loopback, up
// interfaces — not recommended, as it would include usb0). If the named
// interface is absent or down, an error is returned so the caller retries.
func (r *Responder) interfaces() ([]net.Interface, error) {
	if r.ifaceName == "" {
		return nil, nil
	}
	ifi, err := net.InterfaceByName(r.ifaceName)
	if err != nil {
		return nil, fmt.Errorf("interface %q not found: %w", r.ifaceName, err)
	}
	if ifi.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("interface %q is down", r.ifaceName)
	}
	return []net.Interface{*ifi}, nil
}

func (r *Responder) ifaceDesc() string {
	if r.ifaceName == "" {
		return "all interfaces"
	}
	return r.ifaceName
}

// resolveHostname returns the configured override, or the OS hostname.
func (r *Responder) resolveHostname() string {
	if r.hostname != "" {
		return r.hostname
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// signature captures the hostname plus the sorted addresses of the target
// interface(s), so the watcher can detect the changes pion cached at start.
func (r *Responder) signature(ifaces []net.Interface) string {
	if ifaces == nil {
		if r.ifaceName != "" {
			// Named interface not currently resolvable/up.
			return r.resolveHostname() + "|<down>"
		}
		ifaces, _ = net.Interfaces()
	}
	var addrs []string
	for _, ifi := range ifaces {
		aa, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range aa {
			addrs = append(addrs, a.String())
		}
	}
	sort.Strings(addrs)
	return r.resolveHostname() + "|" + strings.Join(addrs, ",")
}

// localNames normalizes a hostname to a single ".local" name, lowercased with
// any trailing dot trimmed. Returns nil for an empty hostname.
func localNames(hostname string) []string {
	h := strings.TrimRight(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if h == "" {
		return nil
	}
	if !strings.HasSuffix(h, ".local") {
		h += ".local"
	}
	return []string{h}
}

// Restart stops the running responder and starts a fresh one from the current
// config. It is how a settings write applies without a process restart: the
// responder's fields are snapshotted at construction, so a config change is
// invisible to an already-running instance.
//
// Stopping first is what makes this safe to call repeatedly — Start() only
// overwrites the singleton pointer, so without the Stop the previous
// responder's watcher goroutine would keep running and keep answering on the
// multicast group with the old name.
func Restart() {
	mu.Lock()
	prev := current
	current = nil
	mu.Unlock()

	prev.Stop() // nil-safe

	if _, err := Start(); err != nil {
		log.Warnf("mdns: restart: %v", err)
	}
}
