package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pi-bmc/nanokvm-app/server/config"
	"github.com/pi-bmc/nanokvm-app/server/logger"
	"github.com/pi-bmc/nanokvm-app/server/middleware"
	"github.com/pi-bmc/nanokvm-app/server/router"
	"github.com/pi-bmc/nanokvm-app/server/service/application"
	"github.com/pi-bmc/nanokvm-app/server/service/autoupdate"
	"github.com/pi-bmc/nanokvm-app/server/service/efivars"
	"github.com/pi-bmc/nanokvm-app/server/service/firmware"
	"github.com/pi-bmc/nanokvm-app/server/service/ipmi"
	"github.com/pi-bmc/nanokvm-app/server/service/mdns"
	"github.com/pi-bmc/nanokvm-app/server/service/network"
	"github.com/pi-bmc/nanokvm-app/server/service/serial"
	sshd "github.com/pi-bmc/nanokvm-app/server/service/ssh"
	"github.com/pi-bmc/nanokvm-app/server/service/usbgadget"
	"github.com/pi-bmc/nanokvm-app/server/telemetry"
	"github.com/pi-bmc/nanokvm-app/server/utils"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
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
)

// networkReadyTimeout caps how long startup waits for the first interface
// configuration pass. It covers a full first DHCP attempt (link wait +
// DISCOVER/REQUEST, ~40s worst case); on timeout the server starts anyway and
// the network manager keeps retrying in the background.
const networkReadyTimeout = 60 * time.Second

func main() {
	initialize()
	defer dispose()

	run()
}

func initialize() {
	log.Printf("NanoKVM BMC %s (commit=%s, built=%s)", version, commit, date)

	// Propagate build-time version to the application service.
	application.Version = version
	telemetry.Version = version

	logger.Init()

	// Apply a soft heap limit so the GC pushes back before the process exhausts
	// memory on this constrained device (no-op if GOMEMLIMIT is set in the env).
	utils.InitGoMemLimit()

	// Initialize OpenTelemetry + Prometheus (no-op when disabled in config).
	if err := telemetry.Init(context.Background()); err != nil {
		log.Printf("telemetry init: %v", err)
	}

	// Start IPMI server on standard port 623
	srv, err := ipmi.Start(623)
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
	if err := firmware.GetController().Init(); err != nil {
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
	autoupdate.Start()

	// Start the SSH server. The image ships no sshd — this is the BMC's only
	// SSH listener, authenticating against the same account as the web UI plus
	// the configured authorized_keys, and running sessions on the shared PTY
	// plumbing the web terminal uses. No-op when ssh.enabled is false.
	if err := sshd.Start(); err != nil {
		log.Printf("SSH server start: %v", err)
	}

	// Start the mDNS responder (advertises <hostname>.local). Replaces
	// avahi-daemon; its watcher brings it up once eth0 has an address.
	if r, err := mdns.Start(); err != nil {
		log.Printf("mDNS start: %v", err)
	} else {
		mdnsResponder = r
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		sig := <-sigChan
		log.Printf("\nReceived signal: %v\n", sig)

		dispose()
		os.Exit(0)
	}()
}

func run() {
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
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	if conf.Authentication == "disable" {
		r.Use(cors.Default())
	}

	// Configure templ renderer with fallback to Gin's default HTML renderer.
	ginHtmlRenderer := r.HTMLRender
	r.HTMLRender = &router.HTMLTemplRenderer{FallbackHtmlRenderer: ginHtmlRenderer}

	router.Init(r)

	httpAddr := fmt.Sprintf(":%d", conf.Port.Http)
	httpsAddr := fmt.Sprintf(":%d", conf.Port.Https)

	if conf.Proto == "https" {
		go func() {
			err := r.RunTLS(httpsAddr, conf.Cert.Crt, conf.Cert.Key)
			if err != nil {
				panic("start https server failed")
			}
		}()

		if err := middleware.ListenAndServeLoopbackHTTPRedirect(
			httpAddr,
			httpsAddr,
			r,
		); err != nil {
			panic("start http server failed")
		}
	} else {
		if err := r.Run(httpAddr); err != nil {
			panic("start http server failed")
		}
	}
}

func dispose() {
	autoupdate.Stop()
	serial.StopCapture()
	sshd.Stop()
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
