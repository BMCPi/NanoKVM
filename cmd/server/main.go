package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pi-bmc/nanokvm-app/api"
	"github.com/pi-bmc/nanokvm-app/pkg/application"
	"github.com/pi-bmc/nanokvm-app/pkg/autoupdate"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/efivars"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/ipmi"
	"github.com/pi-bmc/nanokvm-app/pkg/logger"
	"github.com/pi-bmc/nanokvm-app/pkg/mdns"
	"github.com/pi-bmc/nanokvm-app/pkg/middleware"
	"github.com/pi-bmc/nanokvm-app/pkg/network"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/serial"
	sshd "github.com/pi-bmc/nanokvm-app/pkg/ssh"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
	"github.com/pi-bmc/nanokvm-app/pkg/timesync"
	"github.com/pi-bmc/nanokvm-app/pkg/usbgadget"
	"github.com/pi-bmc/nanokvm-app/pkg/utils"
	"github.com/pi-bmc/nanokvm-app/pkg/video"
	"github.com/pi-bmc/nanokvm-app/pkg/video/rtc"
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
	ipmiServer    *ipmi.Server
	mdnsResponder *mdns.Responder

	// powerCtrl and fwCtrl are the composition root's controllers, built once
	// in initialize() and shared by run() (via deps.Deps) and the IPMI server.
	powerCtrl *power.Controller
	fwCtrl    *firmware.Controller

	// videoHub is the WebRTC hub over the HDMI capture pipeline, or nil on a
	// device with no capture hardware.
	videoHub *rtc.Hub

	// instanceLock holds the abstract unix socket that marks this process as
	// THE server instance; the kernel releases it on any exit path.
	instanceLock net.Listener
)

// lockInstance refuses to start when another server is already running. The
// listeners, the USB gadget and the firmware image staging all assume sole
// ownership, so a second copy — typically started by hand on the console
// while the supervised one is still seeding its boot image — must not run.
func lockInstance() {
	l, err := net.Listen("unix", "@nanokvm-server.instance")
	if err != nil {
		log.Fatalf("another NanoKVM-Server is already running (instance lock: %v) — "+
			"busybox init supervises it; use `killall NanoKVM-Server` to restart it", err)
	}
	instanceLock = l
}

// networkReadyTimeout caps how long startup waits for the first interface
// configuration pass. It covers a full first DHCP attempt (link wait +
// DISCOVER/REQUEST, ~40s worst case); on timeout the server starts anyway and
// the network manager keeps retrying in the background.
const networkReadyTimeout = 60 * time.Second

func main() {
	lockInstance()

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
		log.Printf("server: %v", err)
		return 1
	}
	return 0
}

func initialize(ctx context.Context) {
	log.Printf("NanoKVM BMC %s (commit=%s, built=%s)", version, commit, date)

	// Propagate build-time version to the application service.
	application.Version = version
	telemetry.Version = version

	logger.Init()

	// Apply a soft heap limit so the GC pushes back before the process exhausts
	// memory on this constrained device (no-op if GOMEMLIMIT is set in the env).
	utils.InitGoMemLimit()

	// Initialize OpenTelemetry + Prometheus (no-op when disabled in config).
	if err := telemetry.Init(ctx); err != nil {
		log.Printf("telemetry init: %v", err)
	}

	// Build the composition-root controllers. These replace the old lazy
	// singletons: constructed once here, shared by every subsystem that needs
	// them (IPMI, the auto-update ticker, the HTTP API and UI via deps.Deps).
	cfg := config.GetInstance()
	powerCtrl = power.NewController(cfg.Hardware, cfg.Power)
	fwCtrl = firmware.NewController(cfg)
	videoHub = newVideoHub(cfg)

	// Start IPMI server on standard port 623
	srv, err := ipmi.Start(623, powerCtrl, fwCtrl)
	if err != nil {
		log.Printf("IPMI server failed to start: %v", err)
	} else {
		ipmiServer = srv
	}

	// Build the USB gadget (g0 + all functions + UDC bind) before presenting the
	// firmware image. usbgadget is the sole owner of the gadget configfs — this
	// replaces the old S03usbdev init script — so the host-visible topology and
	// a bound UDC come up independent of firmware image availability.
	if err := usbgadget.Get().Init(); err != nil {
		log.Printf("USB gadget init: %v", err)
	}

	// Configure the host-facing interfaces via netlink: eth0 (static or an
	// in-process DHCP client) and the USB Redfish Host Interface (usb0, static
	// link-local). Started after the gadget so usb0 will register; bring-up
	// runs in goroutines and run() waits on it (bounded) before opening the
	// HTTP listeners. Replaces the S30eth udhcpc script and the build's
	// ifupdown usb0 stanza.
	network.Start()

	// Initialize firmware controller (mount image if available).
	if err := fwCtrl.Init(); err != nil {
		log.Printf("Firmware controller init: %v", err)
	}

	// Mirror the UEFI variable store to durable storage: restore it into the
	// volatile i2c-slave-eeprom at boot and keep it in sync with host writes.
	efivars.GetManager().StartPersistence()

	// Begin the always-on capture of the host's serial console to a bounded
	// file on the data partition, so its boot/crash logs are retained even
	// when no terminal or SOL session is watching. Holds the port open for
	// the server's lifetime; no-op when serial.capture.enabled is false.
	serial.StartCapture()

	// Start the auto-update ticker (no-op when AutoUpdate.Enabled is false).
	autoupdate.Init(fwCtrl)
	autoupdate.Start()

	// Start the SSH server. The image ships no sshd — this is the BMC's only
	// SSH listener, authenticating against the same account as the web UI plus
	// the configured authorized_keys, and running sessions on the shared PTY
	// plumbing the web terminal uses. No-op when ssh.enabled is false.
	if err := sshd.Start(); err != nil {
		log.Printf("SSH server start: %v", err)
	}

	// Start the clock synchronizer (SNTP + HTTP fallback, RTC mirror).
	// Replaces busybox ntpd; retries with backoff until the network is up.
	timesync.Start()

	// Start the mDNS responder (advertises <hostname>.local). Replaces
	// avahi-daemon; its watcher brings it up once eth0 has an address.
	if r, err := mdns.Start(); err != nil {
		log.Printf("mDNS start: %v", err)
	} else {
		mdnsResponder = r
	}
}

// shutdownTimeout bounds the drain of in-flight HTTP requests once the root
// context is cancelled; connections still open after it are closed hard.
const shutdownTimeout = 10 * time.Second

func run(ctx context.Context, stop context.CancelFunc) error {
	// Hold the HTTP/HTTPS listeners until the initial interface configuration
	// pass has been applied (eth0 addressing attempted, RHI address asserted).
	// Bounded so a dead uplink can never wedge boot — on timeout the manager
	// keeps reconfiguring in the background and the server starts regardless.
	network.WaitReady(networkReadyTimeout)

	conf := config.GetInstance()

	gin.SetMode(gin.ReleaseMode)

	// Route gin's request/error logs through the same destination as the rest
	// of the app (the rotating log file when file logging is configured) so
	// nothing writes to a separate, unrotated stream.
	gin.DefaultWriter = logger.Writer()
	gin.DefaultErrorWriter = logger.Writer()
	gin.DisableConsoleColor()

	r := gin.New()
	// Make *gin.Context a real context.Context: Done/Deadline/Err/Value fall
	// back to the request context, so handlers pass `c` directly into
	// ctx-taking code and cancellation propagates when the client disconnects
	// or the server drains during shutdown.
	r.ContextWithFallback = true
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	if conf.Authentication == "disable" {
		r.Use(cors.Default())
	}

	// Telemetry first so the otelgin middleware wraps every route, then the
	// UI (which installs the templ HTML renderer with gin's default as
	// fallback), then every API sub-router.
	d := &deps.Deps{Config: conf, Power: powerCtrl, Firmware: fwCtrl, Video: videoHub}
	telemetry.Routes(r)
	ui.Register(r, d)
	api.Register(r, d)

	httpAddr := fmt.Sprintf(":%d", conf.Port.Http)
	httpsAddr := fmt.Sprintf(":%d", conf.Port.Https)

	// BaseContext hands the root ctx to every accepted connection, so request
	// contexts (and therefore gin contexts) are cancelled when shutdown starts.
	baseCtx := func(net.Listener) context.Context { return ctx }

	var servers []*http.Server
	errCh := make(chan error, 2)

	if conf.Proto == "https" {
		httpsSrv := &http.Server{Addr: httpsAddr, Handler: r, BaseContext: baseCtx}
		servers = append(servers, httpsSrv)
		go func() {
			if err := httpsSrv.ListenAndServeTLS(conf.Cert.Crt, conf.Cert.Key); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("start https server: %w", err)
			}
		}()

		redirectSrv := middleware.NewLoopbackHTTPRedirect(httpAddr, httpsAddr)
		redirectSrv.BaseContext = baseCtx
		servers = append(servers, redirectSrv)
		go func() {
			if err := redirectSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("start http redirect server: %w", err)
			}
		}()
	} else {
		httpSrv := &http.Server{Addr: httpAddr, Handler: r, BaseContext: baseCtx}
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
	log.Printf("shutdown: signal received, draining requests (max %s)", shutdownTimeout)

	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("shutdown: %s: %v", srv.Addr, err)
		}
	}
	return nil
}

// newVideoHub builds the WebRTC hub over this board's HDMI capture pipeline.
//
// The SG2002 backend (pkg/video/cvi) currently provides the ioctl bindings but
// not yet a video.Capturer over them, so the null implementation is what gets
// wired today: the hub builds, /api/vm/video stays registered, and a session
// fails with video.ErrNotSupported saying why -- which the UI can tell apart
// from a server fault, unlike a missing route. Swapping in the real capturer
// here is the only change this function needs.
func newVideoHub(cfg *config.Config) *rtc.Hub {
	capturer := video.Capturer(&video.Unsupported{})

	hub, err := rtc.NewHub(capturer, rtc.Options{
		ICEServers: iceServers(cfg),
		// Everything but the codec is left at the pipeline's own defaults;
		// H.264 is pinned because it is the one codec every browser
		// decodes, and rtc.Hub fixes the track codec at construction.
		Capture: video.Config{Codec: video.CodecH264},
	})
	if err != nil {
		log.Printf("video: hub init: %v", err)
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

func dispose() {
	if videoHub != nil {
		if err := videoHub.Close(); err != nil {
			log.Printf("video: hub close: %v", err)
		}
	}
	autoupdate.Stop()
	serial.StopCapture()
	sshd.Stop()
	timesync.Stop()
	network.Stop()
	if mdnsResponder != nil {
		mdnsResponder.Stop()
	}
	if ipmiServer != nil {
		ipmiServer.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	telemetry.Shutdown(shutdownCtx)

	// Flush and release the rotating log file last, after other subsystems have
	// logged their shutdown.
	_ = logger.Close()
}
