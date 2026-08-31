package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pi-bmc/nanokvm-app/api"
	"github.com/pi-bmc/nanokvm-app/api/redfish"
	"github.com/pi-bmc/nanokvm-app/pkg/app/application"
	"github.com/pi-bmc/nanokvm-app/pkg/app/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/app/autoupdate"
	"github.com/pi-bmc/nanokvm-app/pkg/app/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/app/network"
	"github.com/pi-bmc/nanokvm-app/pkg/app/timesync"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/device/bmcsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/device/hid"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
	"github.com/pi-bmc/nanokvm-app/pkg/device/serial"
	"github.com/pi-bmc/nanokvm-app/pkg/device/usbgadget"
	"github.com/pi-bmc/nanokvm-app/pkg/device/video"
	"github.com/pi-bmc/nanokvm-app/pkg/device/video/rtc"
	"github.com/pi-bmc/nanokvm-app/pkg/device/video/v4l2"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/memlimit"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/middleware"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/sysinfo"
	"github.com/pi-bmc/nanokvm-app/pkg/platform/telemetry"
	"github.com/pi-bmc/nanokvm-app/pkg/protocol/discovery"
	"github.com/pi-bmc/nanokvm-app/pkg/protocol/ipmi"
	sshd "github.com/pi-bmc/nanokvm-app/pkg/protocol/ssh"
	"github.com/pi-bmc/nanokvm-app/ui"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/pion/webrtc/v4"
)

// Set by goreleaser ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	ipmiServer *ipmi.Server

	// powerCtrl and fwCtrl are the composition root's controllers, built once
	// in initialize() and shared by run() (via deps.Deps) and the IPMI server.
	powerCtrl *power.Controller
	fwCtrl    *firmware.Controller

	// authSvc is the one credential store / brute-force guard for the process,
	// built once in initialize() (before the SSH server starts, which needs it
	// directly) and shared by run() via deps.Deps.Auth. auth is a library, not
	// a component, so it is constructed with rootLog untagged.
	authSvc *auth.Service

	// rootLog is the process-wide logger returned by logger.Init, threaded
	// into deps.Deps so handler packages can derive component loggers from
	// it instead of reaching for slog's package-level default.
	rootLog *slog.Logger

	// videoHub is the WebRTC hub over the HDMI capture pipeline, or nil on a
	// device with no capture hardware.
	videoHub *rtc.Hub

	// instanceLock holds the abstract unix socket that marks this process as
	// THE server instance; the kernel releases it on any exit path. It is
	// written here and never read again — the assignment itself is the
	// effect: it keeps the listener reachable (and therefore open) for the
	// process lifetime. If it were a local instead, it would be eligible for
	// GC/close as soon as lockInstance returns, silently dropping the lock.
	instanceLock net.Listener //nolint:unused // holds the socket open for the process lifetime; never read by design
)

// lockInstance refuses to start when another server is already running. The
// listeners, the USB gadget and the capsule volume all assume sole
// ownership, so a second copy — typically started by hand on the console
// while the supervised one is still creating its capsule volume — must not
// run. Returns an error instead of calling log.Fatalf itself so the fatal
// decision stays in main(), where deep-exit requires it.
func lockInstance() error {
	// ListenConfig.Listen (rather than the package-level net.Listen) only to
	// satisfy noctx; the context governs the listen syscall itself and has no
	// bearing on the returned listener's lifetime, which is exactly what we
	// need here — the listener must outlive this function and this context.
	l, err := new(net.ListenConfig).Listen(context.Background(), "unix", "@nanokvm-server.instance")
	if err != nil {
		return fmt.Errorf("another NanoKVM-Server is already running (instance lock: %w) — "+
			"busybox init supervises it; use `killall NanoKVM-Server` to restart it", err)
	}
	instanceLock = l
	return nil
}

// networkReadyTimeout caps how long startup waits for the first interface
// configuration pass. It covers a full first DHCP attempt (link wait +
// DISCOVER/REQUEST, ~40s worst case); on timeout the server starts anyway and
// the network manager keeps retrying in the background.
const networkReadyTimeout = 60 * time.Second

func main() {
	if err := lockInstance(); err != nil {
		log.Fatal(err)
	}

	// realMain keeps deferred cleanup running on every exit path; os.Exit here
	// is the only one, after all defers have unwound.
	os.Exit(realMain())
}

func realMain() int {
	// Root context for the whole process: cancelled on the first SIGINT/
	// SIGTERM/SIGQUIT. Every subsystem goroutine and blocking call hangs off
	// this ctx; run() restores default signal handling once shutdown starts,
	// so a second signal force-kills a wedged shutdown.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	initialize(ctx)
	defer dispose()

	if err := run(ctx, stop); err != nil {
		slog.Error("server exited with an error", slog.Any("err", err))
		return 1
	}
	return 0
}

func initialize(ctx context.Context) {
	// First, before any subsystem can log: until Init runs, slog's default
	// handler writes unleveled text to stderr, which on this device goes
	// nowhere useful. Config is read as part of this (Init needs the level and
	// file), so config's own messages are the only ones that predate it.
	rootLog = logger.Init()

	slog.InfoContext(ctx, "NanoKVM BMC starting",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("built", date))

	// Propagate build-time version to the application service.
	application.Version = version
	telemetry.Version = version

	// Apply a soft heap limit so the GC pushes back before the process exhausts
	// memory on this constrained device (no-op if GOMEMLIMIT is set in the env).
	memlimit.InitGoMemLimit(rootLog)

	// Initialize OpenTelemetry + Prometheus (no-op when disabled in config).
	if err := telemetry.Init(ctx, rootLog.With("component", "telemetry")); err != nil {
		slog.ErrorContext(ctx, "telemetry init failed", slog.Any("err", err))
	}
	// Record a rolling hour of counter samples for the metrics panel's trend
	// charts. No-op when telemetry is off, since there is nothing to sample.
	telemetry.StartSampler(ctx)
	// The BMC's own cpu/memory/disk, for the Server Overview's graphs. Started
	// unconditionally, unlike the sampler above: it reads three files rather
	// than gathering the registry, and the graphs it feeds are what an
	// operator reaches for when the appliance feels slow — which is not a
	// moment they can retroactively enable telemetry for.
	sysinfo.StartResourceSampler(ctx, rootLog.With("component", "sysinfo"))
	// The host's own sensors, pushed into our emulated I2C EEPROM from OP-TEE
	// on the Pi. This is the process's only reader of that record — Redfish
	// and IPMI both read through it — so it must start even on a board with no
	// slave EEPROM, where it simply finds nothing.
	bmcsensor.Default().Start(ctx, rootLog.With("component", "bmcsensor"))

	// Build the composition-root controllers. These replace the old lazy
	// singletons: constructed once here, shared by every subsystem that needs
	// them (IPMI, the auto-update ticker, the HTTP API and UI via deps.Deps).
	cfg := config.GetInstance()
	powerCtrl = power.NewController(cfg.Hardware, cfg.Power, rootLog.With("component", "power"))
	fwCtrl = firmware.NewController(cfg, rootLog.With("component", "firmware"))
	// Untagged: auth is a library, not a component (see deps.Deps.Auth and
	// pkg/auth's package doc). Every caller applies its own component tag to
	// the logger it already holds before reaching auth's methods.
	authSvc = auth.NewService(ctx, rootLog)
	videoHub = newVideoHub(cfg)

	// Restore the persisted host state (staged boot override, host-reported
	// inventory) before anything can serve it — the IPMI server below and
	// the Redfish routes both read it.
	redfish.LoadHostState(rootLog.With("component", "redfish"))

	// Begin the always-on capture of the host's serial console to a bounded
	// file on the data partition, so its boot/crash logs are retained even
	// when no terminal or SOL session is watching. Holds the port open for
	// the server's lifetime; no-op when serial.capture.enabled is false.
	serial.StartCapture(rootLog.With("component", "serial"))

	if cfg.IPMI.Enabled {
		srv, err := ipmi.Start(ctx, cfg, powerCtrl, fwCtrl, rootLog.With("component", "ipmi"))
		if err != nil {
			slog.ErrorContext(ctx, "IPMI server failed to start", slog.Any("err", err))
		} else {
			ipmiServer = srv
		}
	}

	// Build the USB gadget (g0 + all functions + UDC bind) before presenting the
	// capsule volume. usbgadget is the sole owner of the gadget configfs — this
	// replaces the old S03usbdev init script — so the host-visible topology and
	// a bound UDC come up independent of the capsule volume's availability.
	if err := usbgadget.Get().Init(rootLog.With("component", "usbgadget")); err != nil {
		slog.ErrorContext(ctx, "USB gadget init failed", slog.Any("err", err))
	}

	// Configure the host-facing interfaces via netlink: eth0 (static or an
	// in-process DHCP client) and the USB Redfish Host Interface (usb0, static
	// link-local). Started after the gadget so usb0 will register; bring-up
	// runs in goroutines and run() waits on it (bounded) before opening the
	// HTTP listeners. Replaces the S30eth udhcpc script and the build's
	// ifupdown usb0 stanza.
	network.Start(rootLog.With("component", "network"))

	// Create the FMP capsule volume if it does not exist yet and present it on
	// the gadget's lun.0, so the host can pick up staged capsules at its next
	// boot.
	if err := fwCtrl.Init(ctx); err != nil {
		slog.ErrorContext(ctx, "firmware controller init failed", slog.Any("err", err))
	}

	// Start the auto-update ticker (no-op when AutoUpdate.Enabled is false).
	autoupdate.Start(ctx, rootLog.With("component", "autoupdate"))

	// Start the SSH server. The image ships no sshd — this is the BMC's only
	// SSH listener, authenticating against the same account as the web UI plus
	// the configured authorized_keys, and running sessions on the shared PTY
	// plumbing the web terminal uses. No-op when ssh.enabled is false.
	if err := sshd.Start(rootLog.With("component", "ssh"), authSvc); err != nil {
		slog.ErrorContext(ctx, "SSH server start failed", slog.Any("err", err))
	}

	// Start the clock synchronizer (SNTP + HTTP fallback, RTC mirror).
	// Replaces busybox ntpd; retries with backoff until the network is up.
	timesync.Start(ctx, rootLog.With("component", "timesync"))

	// Start the discovery responders (mDNS hostname/service records, SSDP).
	// Replaces avahi-daemon; the watcher brings them up once eth0 has an
	// address. No pointer is kept here: a settings-driven discovery.Restart()
	// can swap in a different instance as the package singleton later, so
	// shutdown goes through discovery.Stop() (which always targets whatever
	// is current) rather than a pointer that could go stale.
	if _, err := discovery.Start(ctx, rootLog.With("component", "discovery")); err != nil {
		slog.ErrorContext(ctx, "discovery start failed", slog.Any("err", err))
	}
}

// shutdownTimeout bounds the drain of in-flight HTTP requests once the root
// context is cancelled; connections still open after it are closed hard.
const shutdownTimeout = 10 * time.Second

// disposeTimeout bounds subsystem cleanup after the drain. It is deliberately
// shorter than the drain: by this point nothing is being served, so time spent
// here is time the supervisor cannot use to restart a process that has already
// decided to exit. See dispose.
const disposeTimeout = 5 * time.Second

func run(ctx context.Context, stop context.CancelFunc) error {
	// Hold the HTTP/HTTPS listeners until the initial interface configuration
	// pass has been applied (eth0 addressing attempted, RHI address asserted).
	// Bounded so a dead uplink can never wedge boot — on timeout the manager
	// keeps reconfiguring in the background and the server starts regardless.
	network.WaitReady(networkReadyTimeout)

	conf := config.GetInstance()

	gin.SetMode(gin.ReleaseMode)

	// gin's own writers are only reached by its internal debug/warning output
	// (route registration, TLS notices) — the request log and panic recovery
	// are slog-backed middleware below. Point them at the same destination so
	// nothing writes to a separate, unrotated stream.
	gin.DefaultWriter = logger.Writer()
	gin.DefaultErrorWriter = logger.Writer()
	gin.DisableConsoleColor()

	r := gin.New()
	// No proxy fronts this server, so trust none: with gin's default
	// (trust-everything) setting, ClientIP() honors X-Forwarded-For from any
	// peer, which let LAN clients spoof their way into IP-keyed behavior —
	// the auth lockout keying and the request log at minimum. The Redfish
	// host-interface check parses the TCP source itself and never depended
	// on this, but every ClientIP() consumer gets the honest address now.
	if err := r.SetTrustedProxies(nil); err != nil {
		return fmt.Errorf("configure trusted proxies: %w", err)
	}
	// Make *gin.Context a real context.Context: Done/Deadline/Err/Value fall
	// back to the request context, so handlers pass `c` directly into
	// ctx-taking code and cancellation propagates when the client disconnects
	// or the server drains during shutdown.
	r.ContextWithFallback = true
	// Order matters and is load-bearing:
	//   1. otelgin, outermost, so its span covers the whole request AND is
	//      readable by everything below it. It restores c.Request on the way
	//      out, so middleware registered outside it sees no span — which is
	//      how the request log silently loses its trace_id if this moves.
	//   2. Recovery, so a panic anywhere below becomes a 500 and one error
	//      record, logged inside the span.
	//   3. RequestLogger, innermost of the three, so its latency covers the
	//      handler rather than the middleware around it.
	telemetry.Middleware(r)
	r.Use(middleware.Recovery(rootLog.With("component", "http")))
	r.Use(middleware.RequestLogger(rootLog.With("component", "http")))
	// Seeds pkg/middleware's shared, caller-independent log sites (currently
	// just ParseJWT's debug line) with one fixed identity, so it does not end
	// up stamped with whichever of api/api.go, ui/ui.go, api/auth or
	// api/redfish happens to construct CheckToken/ResolveAuth last. See
	// middleware.SetLogger's doc comment.
	middleware.SetLogger(rootLog.With("component", "http"))
	if conf.Authentication == "disable" {
		r.Use(cors.Default())
	}

	// Telemetry first so the otelgin middleware wraps every route, then the
	// UI (which installs the templ HTML renderer with gin's default as
	// fallback), then every API sub-router.
	// The HID gadget driver opens its character devices lazily, so building it
	// here costs nothing on a board whose gadget is not configured; the input
	// handlers report that condition instead.
	hidCtrl := hid.NewController(rootLog.With("component", "hid"))
	defer hidCtrl.Close()

	d := &deps.Deps{
		Ctx:      ctx,
		Log:      rootLog,
		Config:   conf,
		Power:    powerCtrl,
		Firmware: fwCtrl,
		Auth:     authSvc,
		Video:    videoHub,
		HID:      hidCtrl,
	}
	telemetry.Routes(r)
	ui.Register(r, d)
	api.Register(r, d)

	httpAddr := fmt.Sprintf(":%d", conf.Port.HTTP)
	httpsAddr := fmt.Sprintf(":%d", conf.Port.HTTPS)

	// BaseContext hands the root ctx to every accepted connection, so request
	// contexts (and therefore gin contexts) are cancelled when shutdown starts.
	baseCtx := func(net.Listener) context.Context { return ctx }

	var servers []*http.Server
	errCh := make(chan error, 2)

	// A configured-but-unprovisioned HTTPS setup used to be fatal: the only
	// caller of GenerateCert is the web UI's TLS toggle, so a device that
	// booted with proto: https and no certificate on disk exited instead of
	// serving anything — including the UI an operator would need to fix it.
	// Provision on demand, and if that fails, serve plaintext rather than
	// nothing.
	useTLS := conf.Proto == "https" && ensureServerCert(conf)

	if useTLS {
		httpsSrv := &http.Server{
			Addr: httpsAddr, Handler: r, BaseContext: baseCtx,
			// Bounds only the time to read the request line + headers, so an
			// idle half-open connection cannot pin a goroutine (Slowloris).
			// This is the web UI's server, including WebSocket upgrades
			// (serial console) and WebRTC signalling: those connections
			// complete their header phase like any other request and are
			// then long-lived on the handler side, which this does not
			// touch — deliberately no ReadTimeout/WriteTimeout here, as
			// either would cut an established stream. 30s matches the
			// existing precedent in pkg/middleware/loopback_http.go.
			ReadHeaderTimeout: 30 * time.Second,
			// IdleTimeout bounds a keep-alive connection sitting between
			// requests. It was previously unset, and with no ReadTimeout
			// either (its own fallback) that left idle keep-alives with no
			// timeout at all -- only the header-read phase was bounded.
			IdleTimeout: 120 * time.Second,
		}
		servers = append(servers, httpsSrv)
		go func() {
			if err := httpsSrv.ListenAndServeTLS(conf.Cert.Crt, conf.Cert.Key); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("start https server: %w", err)
			}
		}()

		// The plain port cannot be redirect-only: the managed host's
		// firmware speaks DSP0270 plain HTTP on the RHI subnet and cannot
		// follow a redirect, so a blanket 307 severs the whole host sync
		// (identity, thermal, boot-override consumption). Serve RHI-sourced
		// requests directly, redirect everyone else.
		redirectSrv := middleware.NewPlainHTTPServer(httpAddr, httpsAddr, conf.Network.RHI.Address, r)
		redirectSrv.BaseContext = baseCtx
		servers = append(servers, redirectSrv)
		go func() {
			if err := redirectSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("start http redirect server: %w", err)
			}
		}()
	} else {
		httpSrv := &http.Server{
			Addr: httpAddr, Handler: r, BaseContext: baseCtx,
			// See the httpsSrv comment above: bounds header reading only,
			// not the lifetime of an upgraded WebSocket/WebRTC-signalling
			// connection served on this same listener when TLS is off.
			ReadHeaderTimeout: 30 * time.Second,
			// IdleTimeout: see the httpsSrv comment above -- same gap, same fix.
			IdleTimeout: 120 * time.Second,
		}
		servers = append(servers, httpSrv)
		go func() {
			if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("start http server: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Shutdown path: restore default signal handling first so a second signal
	// force-kills instead of being swallowed while we drain.
	stop()
	slog.Info("shutdown: signal received, draining requests",
		slog.Duration("timeout", shutdownTimeout))

	// Persist before draining: host reports save on a debounce, and the
	// in-flight timer dies with the process.
	redfish.FlushHostState(rootLog.With("component", "redfish"))

	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(drainCtx); err != nil {
			slog.Error("shutdown: server drain failed",
				slog.String("addr", srv.Addr), slog.Any("err", err))
		}
	}
	return nil
}

// newVideoHub builds the WebRTC hub over this board's HDMI capture pipeline.
//
// The SG2002 backend is used when its devices are there, and the null
// implementation when they are not. That fallback is not defensive padding: the
// soph-media modules are out-of-tree and loaded separately from the app, a board
// may have no HDMI bridge fitted at all, and none of that is a reason for a
// management controller to fail to boot. When it falls back, /api/vm/video stays
// registered and a session fails with video.ErrNotSupported saying why -- which
// the UI can tell apart from a server fault, unlike a missing route.
func newVideoHub(cfg *config.Config) *rtc.Hub {
	capturer := video.Capturer(&video.Unsupported{})
	if c, err := v4l2.Open(rootLog.With("component", "video")); err != nil {
		slog.Warn("video: capture unavailable, serving without it", slog.Any("err", err))
	} else {
		capturer = c
	}

	hub, err := rtc.NewHub(capturer, rtc.Options{
		ICEServers: iceServers(cfg),
		// Everything but the codec is left at the pipeline's own defaults;
		// H.264 is pinned because it is the one codec every browser
		// decodes, and rtc.Hub fixes the track codec at construction.
		Capture: video.Config{Codec: video.CodecH264},
		Log:     rootLog.With("component", "rtc"),
	})
	if err != nil {
		slog.Error("video: hub init failed", slog.Any("err", err))
		return nil
	}
	return hub
}

// iceServers maps the configured STUN/TURN servers onto pion's form. Both are
// empty by default -- see pkg/config/default.go for why a management controller
// does not contact a public STUN server on its own.
func iceServers(cfg *config.Config) []webrtc.ICEServer {
	var servers []webrtc.ICEServer
	if cfg.Stun != "" {
		servers = append(servers, webrtc.ICEServer{URLs: []string{"stun:" + cfg.Stun}})
	}
	if cfg.Turn.TurnAddr != "" {
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{"turn:" + cfg.Turn.TurnAddr},
			Username:   cfg.Turn.TurnUser,
			Credential: cfg.Turn.TurnCred,
		})
	}
	return servers
}

// dispose releases every subsystem, and gives up if that takes too long.
//
// The bound is the point. Cleanup runs in series, video first, and closing the
// video hub joins the capture supervisor -- which may be part-way through
// bringing a pipeline up, inside an ioctl on a driver that has stopped
// answering. There is no way to interrupt that from here.
//
// disposeAll wraps each subsystem's Stop in its own stopBounded call rather
// than relying solely on the outer bound below: a wedged video Close (or any
// other subsystem) used to be able to exhaust the whole budget by itself,
// silently skipping every subsystem queued up after it. The observed shape of
// that bug was an app that logged "draining requests", kept accepting SSH
// connections, and ignored every SIGTERM after -- so `killall NanoKVM-Server`
// looked like it did nothing and the supervisor never got the chance to
// restart it. The runBounded call here remains as a final backstop for the
// pathological case where several subsystems are wedged at once and their
// individual bounds sum past disposeTimeout.
//
// Exiting with a subsystem un-stopped is fine here in a way it would not be in
// a general-purpose program: this process is supervised and about to be
// replaced, and the kernel reclaims descriptors, mappings and modules on exit
// regardless of whether we asked nicely first.
func dispose() {
	if !runBounded(disposeTimeout, disposeAll) {
		slog.Warn("shutdown: cleanup still running, exiting anyway",
			slog.Duration("timeout", disposeTimeout))
	}
}

// runBounded runs fn and reports whether it finished within timeout.
//
// On expiry fn is abandoned rather than cancelled -- it is blocked in a syscall
// that will not return, which is the whole reason for the bound -- so this is
// only sound where the caller is about to exit the process.
func runBounded(timeout time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// disposeStopTimeout bounds each individual subsystem Stop call in disposeAll,
// in place of the single disposeTimeout budget the whole function used to
// share. That shared budget meant the first slow Stop starved every
// subsystem still waiting behind it -- since Stop calls run in series, a
// single wedged one could exhaust disposeTimeout before the rest even
// started. Giving each its own bound trades a smaller worst-case-per-subsystem
// guarantee for a much better one overall: every subsystem gets its own
// attempt regardless of how its predecessor behaved. The outer
// runBounded(disposeTimeout, disposeAll) call in dispose remains as a final
// backstop for the pathological case where several subsystems are wedged at
// once.
const disposeStopTimeout = 2 * time.Second

// stopBounded runs one subsystem's Stop in a goroutine and gives up waiting
// after disposeStopTimeout, logging a warning that names the subsystem rather
// than letting it block whatever disposeAll would otherwise run next. Like
// runBounded, fn is abandoned rather than cancelled on expiry -- Stop takes no
// ctx to cancel with -- so this is only sound because disposeAll is itself
// called from a process that is about to exit either way.
func stopBounded(name string, fn func()) {
	if !runBounded(disposeStopTimeout, fn) {
		slog.Warn("shutdown: subsystem stop timed out, continuing",
			slog.String("subsystem", name), slog.Duration("timeout", disposeStopTimeout))
	}
}

func disposeAll() {
	if videoHub != nil {
		stopBounded("video", func() {
			if err := videoHub.Close(); err != nil {
				slog.Error("video: hub close failed", slog.Any("err", err))
			}
		})
	}
	stopBounded("autoupdate", autoupdate.Stop)
	stopBounded("serial capture", serial.StopCapture)
	// Closes the broker singleton itself, not just the always-on capture
	// session StopCapture disconnects above: Close is what actually shuts the
	// port and releases its sessions map, and was never called at shutdown
	// before this. Safe to call even though StopCapture already drove the
	// session count to zero -- see Broker.Close's doc comment on idempotency.
	stopBounded("serial broker", serial.GetBroker().Close)
	stopBounded("ssh", sshd.Stop)
	stopBounded("timesync", timesync.Stop)
	stopBounded("network", network.Stop)
	stopBounded("discovery", discovery.Stop)
	if ipmiServer != nil {
		stopBounded("ipmi", ipmiServer.Stop)
	}
	// Power is closed late, after every other subsystem that might still be
	// mid-sequence on it (IPMI's chassis control, a Redfish power action) has
	// already stopped: releasing the power-LED GPIO line out from under a
	// still-running sequence would be worse than leaving it held for a
	// process that is about to exit anyway.
	stopBounded("power", powerCtrl.Close)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	telemetry.Shutdown(shutdownCtx)

	// Flush and release the rotating log file last, after other subsystems have
	// logged their shutdown.
	_ = logger.Close()
}

// ensureServerCert reports whether the configured TLS material is usable,
// generating a self-signed pair when it is absent.
//
// It returns false rather than failing the boot: the certificate is how the
// operator reaches the machine, so a device that cannot produce one is far
// better off answering over plaintext — where the UI can be used to fix it —
// than exiting into a reboot loop with no management path at all.
func ensureServerCert(conf *config.Config) bool {
	crt, key := conf.Cert.Crt, conf.Cert.Key
	if crt == "" || key == "" {
		slog.Error("https configured but cert/key paths are empty; falling back to http")
		return false
	}

	_, crtErr := os.Stat(crt)
	_, keyErr := os.Stat(key)
	if crtErr == nil && keyErr == nil {
		return true
	}
	if crtErr != nil && !os.IsNotExist(crtErr) {
		slog.Error("https configured but certificate is unreadable; falling back to http",
			slog.String("path", crt), slog.Any("err", crtErr))
		return false
	}

	slog.Info("https configured but no certificate present; generating a self-signed pair",
		slog.String("path", crt))
	if err := middleware.GenerateCert(rootLog); err != nil {
		slog.Error("could not generate a server certificate; falling back to http", slog.Any("err", err))
		return false
	}
	if _, err := os.Stat(crt); err != nil {
		// GenerateCert writes to fixed paths; a configured pair pointing
		// somewhere else is still unprovisioned afterwards.
		slog.Error("generated certificate is not at the configured path; falling back to http",
			slog.String("path", crt), slog.Any("err", err))
		return false
	}
	return true
}
