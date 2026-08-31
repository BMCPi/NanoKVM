// Package discovery owns the lifecycle of the two responders that make this
// BMC findable on the LAN without an operator already knowing its address:
// DNS-SD/mDNS (pkg/discovery/mdns, hostname + _redfish._tcp/_http._tcp/etc.
// service records) and SSDP (pkg/discovery/ssdp, the UPnP-style
// announce-and-respond protocol Redfish discovery tooling speaks per
// DSP0263). Both sub-packages are deliberately socket-free service/message
// builders; this package is where they meet a network.
//
// The two responders are independent by design: one failing to bind (most
// often because the target interface is not up yet) must never stop the
// other from serving, so each is started and torn down on its own and a
// failure from one is only ever logged, never propagated as a reason to skip
// the other.
//
// pion caches an interface's addresses (and skips down interfaces) at
// responder-start, and SSDP's Location header is a URL burned in at Start
// time too, so a background watcher restarts both responders whenever the
// hostname or either scoped interface's addresses change — e.g. once eth0
// obtains a DHCP lease, or its lease changes. This mirrors JetKVM restarting
// mDNS on network-state changes, extended to cover SSDP's address-bearing
// Location as well.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/discovery/mdns"
	"github.com/pi-bmc/nanokvm-app/pkg/discovery/ssdp"
	"github.com/pi-bmc/nanokvm-app/pkg/identity"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
)

// watchInterval is how often the watcher re-checks the hostname / interface
// addresses and restarts the responders if they changed.
const watchInterval = 15 * time.Second

// Responder is a running set of discovery responders (mDNS, SSDP, or both,
// per config).
type Responder struct {
	mu sync.Mutex

	mdnsR *mdns.Responder
	ssdpR *ssdp.Responder

	// mdnsEnabled/ssdpEnabled are snapshotted at construction, like every
	// other field below: a live responder's fields never change underneath
	// it, so applying a settings change means Restart, not mutation.
	mdnsEnabled bool
	ssdpEnabled bool

	ifaceName     string   // mDNS-scoped interface
	ssdpIfaceName string   // SSDP-scoped interface; may differ from ifaceName
	hostname      string   // configured override, or "" to use the OS hostname
	names         []string // currently advertised mDNS names (e.g. "nanokvm.local")
	lastSig       string   // hostname + interface addresses at the last (re)start

	// ctx is the process-lifetime context Start was given, passed to both
	// responders' Start so their propagation actually reaches something --
	// see startMDNSLocked/startSSDPLocked.
	ctx context.Context

	// stopped fences startLocked once Stop has run. Without it, a watcher
	// tick can read mu, release it, and race a concurrent Restart(): Stop()
	// closes stopCh and nils/tears down both responders, then the tick's
	// already-in-flight start() reacquires mu and binds fresh sockets onto
	// this now-retired Responder. Nothing owns them afterwards — the
	// watcher's next iteration selects stopCh and returns, leaking a bound
	// mDNS socket, a bound SSDP socket, and the dnssd goroutine for the rest
	// of the process's life. Checking stopped as the first thing inside
	// startLocked (same mutex Stop uses to set it) closes that gap: whichever
	// of Stop/start wins the mu race, the other observes its result.
	stopped bool

	stopCh   chan struct{}
	stopOnce sync.Once

	log *slog.Logger
}

// The process-wide singleton, so the vm info endpoint and the settings page
// can report the advertised name without threading the pointer through every
// service.
var (
	mu      sync.Mutex
	current *Responder
)

// pkgLogHolder is pkg/discovery's holder for the "discovery" component
// logger. It exists because Start returns (nil, nil) with no Responder at
// all when both mDNS and SSDP are disabled — so a later Restart() (a
// settings write re-enabling discovery) would otherwise have nowhere to draw
// the logger from. Start always Sets it, so its real, component-tagged
// logger wins no matter what already Get'ed the holder first — see
// logger.Holder's doc comment for why a sync.Once-guarded var would get this
// wrong.
var pkgLogHolder logger.Holder

// pkgLog returns the package's component logger, defaulting to the process
// logger if Start has not run yet (a discovery_test.go unit test exercising
// a Responder built directly).
func pkgLog() *slog.Logger {
	return pkgLogHolder.Get()
}

// procCtx is the ctx most recently given to Start, guarded by mu (the same
// lock that guards current) so Restart can reuse it instead of falling back
// to context.Background() -- mirrors pkgLogHolder's reasoning for the logger.
var procCtx context.Context

// Start builds and starts the responders from config, stores the result as
// the process singleton, and launches the restart watcher. It returns
// (nil, nil) when both mDNS and SSDP are disabled — a legitimate deployment
// choice, not an error, and one that must touch no socket at all.
//
// SSDP additionally requires Redfish.Enabled: its search target
// (urn:dmtf-org:service:redfish-rest:*) is Redfish-specific and means nothing
// when Redfish itself is off.
//
// An initial bind failure on either responder is not fatal — the watcher
// retries (the target interface may not be up yet).
func Start(ctx context.Context, log *slog.Logger) (*Responder, error) {
	log = logger.Or(log)
	pkgLogHolder.Set(log)
	if ctx == nil {
		ctx = context.Background()
	}

	mu.Lock()
	procCtx = ctx
	mu.Unlock()

	cfg := config.GetInstance()
	disc := cfg.Discovery

	mdnsOn := disc.MDNS.Enabled
	ssdpOn := disc.SSDP.Enabled && cfg.Redfish.Enabled

	if !mdnsOn && !ssdpOn {
		log.Info("discovery: mdns and ssdp both disabled")
		return nil, nil
	}

	r := &Responder{
		mdnsEnabled:   mdnsOn,
		ssdpEnabled:   ssdpOn,
		ifaceName:     disc.MDNS.Interface,
		ssdpIfaceName: disc.SSDP.Interface,
		hostname:      disc.MDNS.Hostname,
		ctx:           ctx,
		stopCh:        make(chan struct{}),
		log:           log,
	}
	if err := r.start(); err != nil {
		log.Warn("discovery: initial start failed (watcher will retry)", slog.Any("err", err))
	}
	go r.watch()

	mu.Lock()
	current = r
	mu.Unlock()
	return r, nil
}

// Advertised returns the name the running responder publishes over mDNS
// (e.g. "nanokvm.local") and whether it is active. Used by the vm info
// endpoint and the settings page in place of the old avahi PID-file probe.
// It reports the mDNS name only — SSDP has no equivalent human-facing name to
// show.
func Advertised() (string, bool) {
	mu.Lock()
	r := current
	mu.Unlock()
	if r == nil {
		return "", false
	}
	return r.Name()
}

// Name returns the primary advertised mDNS name when that responder is
// running. It answers false whenever the mDNS responder specifically is not
// up, even if SSDP is — Advertised()'s callers show a hostname, not a
// protocol-independent "is discovery on" flag.
func (r *Responder) Name() (string, bool) {
	r.mu.Lock()
	mr := r.mdnsR
	r.mu.Unlock()
	if mr == nil {
		return "", false
	}
	return mr.Name()
}

// Stop tears down whichever responders are running, plus the watcher. Safe to
// call more than once.
func (r *Responder) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })

	r.mu.Lock()
	mr, sr := r.mdnsR, r.ssdpR
	r.mdnsR, r.ssdpR = nil, nil
	r.stopped = true
	r.mu.Unlock()

	// Both Stop methods dereference their receiver's fields without a nil
	// guard (they're never called on a nil *Responder elsewhere in their own
	// packages), so the nil checks belong here.
	if mr != nil {
		mr.Stop()
	}
	if sr != nil {
		sr.Stop()
	}
}

func (r *Responder) start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked()
}

func (r *Responder) startLocked() error {
	// A retired Responder must never bind another socket — see the stopped
	// field's doc comment for the leak this closes.
	if r.stopped {
		return nil
	}

	// Stop whatever is currently running first (this doubles as Restart).
	if r.mdnsR != nil {
		r.mdnsR.Stop()
		r.mdnsR = nil
	}
	if r.ssdpR != nil {
		r.ssdpR.Stop()
		r.ssdpR = nil
	}

	names := localNames(r.resolveHostname())
	if len(names) == 0 {
		return fmt.Errorf("no hostname to advertise")
	}
	r.names = names

	cfg := config.GetInstance()

	// Each responder is started independently and its failure recorded, but
	// never aborts the other — an interface that only one of them is scoped
	// to being down must not silence the one that isn't.
	var errs []string
	if r.mdnsEnabled {
		if err := r.startMDNSLocked(cfg, names[0]); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if r.ssdpEnabled {
		if err := r.startSSDPLocked(cfg); err != nil {
			errs = append(errs, err.Error())
		}
	}

	// The signature drives the watcher's restart decision. A resolution
	// failure here just becomes the "<down>" sentinel rather than aborting —
	// each responder above has already handled its own failure on its own
	// terms.
	if ifaces, err := r.interfaces(); err != nil {
		r.lastSig = r.resolveHostname() + "|<down>"
	} else {
		r.lastSig = r.signature(ifaces)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// startMDNSLocked builds and starts the DNS-SD responder from config. Ports,
// enabled subsystems and the BMC's stable UUID all come from the live config
// snapshot rather than being cached on Responder, so a Restart always
// advertises the current deployment.
func (r *Responder) startMDNSLocked(cfg *config.Config, host string) error {
	svcs := mdns.Services(mdns.Inputs{
		Proto:          cfg.Proto,
		HTTPPort:       cfg.Port.HTTP,
		HTTPSPort:      cfg.Port.HTTPS,
		RedfishEnabled: cfg.Redfish.Enabled,
		SSHEnabled:     cfg.SSH.Enabled,
		SSHPort:        cfg.SSH.Port,
		UUID:           identity.BMCUUID(),
	})
	mr := mdns.New(host, r.ifaceName, svcs)
	if err := mr.Start(r.ctx); err != nil {
		return fmt.Errorf("mdns: %w", err)
	}
	r.mdnsR = mr
	r.log.Info("discovery: mdns advertising",
		slog.String("host", host), slog.String("iface", ifaceDesc(r.ifaceName)))
	return nil
}

// startSSDPLocked builds and starts the SSDP responder from config. Location
// is the service root URL the AL header advertises: config.Proto's scheme
// plus the BMC's current address on the scoped interface plus the fixed
// Redfish service root path — which is exactly what changes when that
// interface's address does, and exactly why the watcher restarts SSDP
// alongside mDNS on such a change rather than leaving it to advertise a URL
// nothing answers on any more.
func (r *Responder) startSSDPLocked(cfg *config.Config) error {
	addr, err := interfaceAddress(r.ssdpIfaceName)
	if err != nil {
		return fmt.Errorf("ssdp: %w", err)
	}
	minor, err := redfishMinor()
	if err != nil {
		return fmt.Errorf("ssdp: %w", err)
	}

	location := fmt.Sprintf("%s://%s/redfish/v1/", cfg.Proto, ssdpHostPort(cfg, addr))
	sr := ssdp.New(ssdp.Config{
		Iface:    r.ssdpIfaceName,
		UUID:     identity.BMCUUID(),
		Location: location,
		Minor:    minor,
		MaxAge:   cfg.Discovery.SSDP.MaxAge,
	})
	if err := sr.Start(r.ctx); err != nil {
		return fmt.Errorf("ssdp: %w", err)
	}
	r.ssdpR = sr
	r.log.Info("discovery: ssdp advertising",
		slog.String("location", location), slog.String("iface", ifaceDesc(r.ssdpIfaceName)))
	return nil
}

// ssdpHostPort returns addr, or "addr:port" when cfg's listening port for its
// own Proto is not that scheme's default (443 for https, 80 for http). A
// Redfish discovery client dials the Location/AL URL exactly as advertised —
// cmd/server listens on 8443, not 443, in the shipped config, so an omitted
// port there would send every such client to a closed port. The default
// port is still omitted, matching how a browser or curl treats a bare
// https:// URL, rather than needlessly spelling out ":443".
func ssdpHostPort(cfg *config.Config, addr string) string {
	port, schemeDefault := cfg.Port.HTTPS, 443
	if cfg.Proto != "https" {
		port, schemeDefault = cfg.Port.HTTP, 80
	}
	if port == 0 || port == schemeDefault {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

// redfishMinor parses the minor component out of identity.RedfishProtocolVersion
// (e.g. "1.13.0" -> 13) for SSDP's versioned search target
// (urn:dmtf-org:service:redfish-rest:1:<minor>), so a bumped protocol version
// is reflected here automatically instead of needing a second constant kept
// in sync by hand. Failing on a malformed version is deliberate: advertising
// minor 0 (or any other wrong guess) would claim a capability set the BMC
// does not have, which is worse than SSDP not starting at all.
func redfishMinor() (int, error) {
	parts := strings.SplitN(identity.RedfishProtocolVersion, ".", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("malformed RedfishProtocolVersion %q", identity.RedfishProtocolVersion)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("malformed RedfishProtocolVersion %q: %w", identity.RedfishProtocolVersion, err)
	}
	return minor, nil
}

// interfaceAddress returns a usable unicast IPv4 address on the named
// interface, for building SSDP's Location. A link-local (169.254/16)
// address is accepted only as a fallback when nothing else is present: it is
// a real, dialable address (unlike an empty interface name, which this
// function refuses outright), just not the one a DHCP-assigned network
// expects to see advertised once a real lease exists — which is exactly the
// state the watcher's restart-on-change corrects.
func interfaceAddress(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("no interface configured")
	}
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return "", fmt.Errorf("interface %q not found: %w", name, err)
	}
	if ifi.Flags&net.FlagUp == 0 {
		return "", fmt.Errorf("interface %q is down", name)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", fmt.Errorf("interface %q addresses: %w", name, err)
	}

	var linkLocal string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue // IPv6 Location would need bracketing; not needed on this deployment
		}
		if ip4.IsLinkLocalUnicast() {
			if linkLocal == "" {
				linkLocal = ip4.String()
			}
			continue
		}
		return ip4.String(), nil
	}
	if linkLocal != "" {
		return linkLocal, nil
	}
	return "", fmt.Errorf("interface %q has no usable address", name)
}

// watch restarts the responders whenever the hostname or either scoped
// interface's addresses change (both responders cache what they need at
// start), and retries while nothing is running (e.g. eth0 still coming up).
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
			running := r.mdnsR != nil || r.ssdpR != nil
			r.mu.Unlock()

			if !changed && running {
				continue
			}
			if err := r.start(); err != nil {
				r.log.Debug("discovery: restart on change failed (will retry)", slog.Any("err", err))
			} else {
				r.log.Debug("discovery: (re)started after network/hostname change")
			}
		}
	}
}

// interfaces resolves the mDNS and SSDP interface names to a []net.Interface
// for the watcher's signature. Both are watched, not just mDNS's, because
// SSDP's Location embeds the address of its own scoped interface — if it
// differs from mDNS's, an address change there must trigger a restart just
// the same. An empty name is skipped rather than expanded to "all
// interfaces": with two independently-configured names there is no single
// coherent meaning for "watch everything" the way the single-interface old
// mdns package had.
func (r *Responder) interfaces() ([]net.Interface, error) {
	var names []string
	for _, n := range []string{r.ifaceName, r.ssdpIfaceName} {
		if n == "" {
			continue
		}
		dup := false
		for _, seen := range names {
			if seen == n {
				dup = true
				break
			}
		}
		if !dup {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}

	out := make([]net.Interface, 0, len(names))
	for _, name := range names {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			return nil, fmt.Errorf("interface %q not found: %w", name, err)
		}
		if ifi.Flags&net.FlagUp == 0 {
			return nil, fmt.Errorf("interface %q is down", name)
		}
		out = append(out, *ifi)
	}
	return out, nil
}

func ifaceDesc(name string) string {
	if name == "" {
		return "all interfaces"
	}
	return name
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

// signature captures the hostname plus the sorted addresses of the watched
// interface(s), so the watcher can detect the changes each responder cached
// at its own start.
func (r *Responder) signature(ifaces []net.Interface) string {
	if ifaces == nil {
		var err error
		ifaces, err = r.interfaces()
		if err != nil {
			// A named interface is not currently resolvable/up.
			return r.resolveHostname() + "|<down>"
		}
		if ifaces == nil {
			ifaces, _ = net.Interfaces()
		}
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

// Restart stops whichever responders are running and starts fresh ones from
// the current config. It is how a settings write applies without a process
// restart: a Responder's fields are snapshotted at construction, so a config
// change is invisible to an already-running instance.
//
// Stopping first is what makes this safe to call repeatedly — Start() only
// overwrites the singleton pointer, so without the Stop the previous
// responders' watcher goroutine would keep running and keep answering on
// their multicast groups with the old name/services.
//
// Reuses the package's stored logger and process ctx rather than falling
// back to bare defaults — both stuck the first time Start ran, whether or not
// discovery was enabled then.
func Restart() {
	log := pkgLog()

	mu.Lock()
	prev := current
	current = nil
	ctx := procCtx
	mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	prev.Stop() // nil-safe

	if _, err := Start(ctx, log); err != nil {
		log.Warn("discovery: restart failed", slog.Any("err", err))
	}
}

// Stop tears down the current singleton responder, if any, and clears it.
//
// main holds no responder pointer of its own for exactly this reason: Start()
// returns one, but a settings-driven Restart() later swaps in a different
// instance as the package singleton without main ever learning about it. A
// pointer held from the original Start() would then call Stop() on an
// already-retired Responder at shutdown — a correctly-behaving no-op that
// nonetheless leaves the real, live responder running, so it exits without
// sending its DNS-SD goodbyes or ssdp:byebye. Those must go out before the
// process closes, or discovery caches keep advertising a dead BMC for the
// full max-age. Routing shutdown through the singleton here, instead of a
// remembered pointer, is what keeps that guarantee true across any number of
// Restarts. Safe to call when nothing was ever started.
func Stop() {
	mu.Lock()
	r := current
	current = nil
	mu.Unlock()

	r.Stop() // nil-safe
}
