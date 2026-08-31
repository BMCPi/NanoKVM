// Package ipmi serves IPMI-over-LAN by adapting this BMC's controllers onto
// the github.com/bougou/go-ipmi server framework.
//
// The framework owns everything protocol-shaped: RMCP/RMCP+ session
// establishment (cipher suites 3 and 17), packet integrity/confidentiality,
// user authentication, command dispatch, SOL payload sequencing, and the
// SDR/FRU storage commands. This package owns everything board-shaped: a
// hal.HAL implementation backed by the power controller, the serial broker,
// the OP-TEE sensor reader, and the Redfish boot-override state, plus the
// OEM commands for the firmware capsule flow.
package ipmi

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/server"
	"github.com/bougou/go-ipmi/pkg/transport/udp"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/pkg/bmcsensor"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/firmware"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/serial"
	"github.com/pi-bmc/nanokvm-app/pkg/telemetry"
)

// bmcGUID is the fixed identifier this BMC has always reported through Get
// System GUID. Kept byte-for-byte across the framework migration so clients
// that cached it do not see a different BMC.
var bmcGUID = [16]byte{
	0x4e, 0x61, 0x6e, 0x6f, 0x4b, 0x56, 0x4d, 0x2d,
	0x42, 0x4d, 0x43, 0x2d, 0x47, 0x55, 0x49, 0x44,
}

// deviceInfo carries forward the identity the old Get Device ID handler
// reported, except AdditionalDeviceSupport: the old value (0x2F) claimed SEL,
// SDR and FRU before any of them existed. 0x8B is what this BMC actually
// serves — chassis device, FRU inventory, SDR repository, sensor device.
var deviceInfo = bmc.DeviceInfo{
	DeviceID:                0x20,
	DeviceRevision:          0x01,
	FirmwareMajor:           2,
	FirmwareMinor:           0,
	IPMIVersion:             0x20,
	ManufacturerID:          0x0002A2,
	ProductID:               0x0001,
	AdditionalDeviceSupport: 0x8B,
}

// Server is the running IPMI service.
type Server struct {
	srv    *server.Server
	cancel context.CancelFunc
	done   chan struct{}
	addr   net.Addr
}

// deps is everything the service needs, as interfaces so tests can substitute
// fakes. Start wires the real controllers in.
type deps struct {
	port     int
	username string
	password string
	power    powerController
	firmware firmwareStatus
	broker   consoleBroker
	sensors  sensorSource
}

// Start creates and starts the IPMI server per cfg.IPMI. ctx is the process
// root context: IPMI has no per-request context of its own — a command
// arrives as a datagram and the requester does not stay on the line — so it
// is what carries telemetry and bounds the detached power sequences.
func Start(ctx context.Context, cfg *config.Config, powerCtrl *power.Controller, fwCtrl *firmware.Controller) (*Server, error) {
	return startServer(ctx, deps{
		port:     cfg.IPMI.Port,
		username: cfg.IPMI.Username,
		password: cfg.IPMI.Password,
		power:    powerCtrl,
		firmware: fwCtrl,
		broker:   serial.GetBroker(),
		sensors:  bmcsensor.NewReader(),
	})
}

func startServer(ctx context.Context, d deps) (*Server, error) {
	h := newHAL(ctx, d.power, d.broker, d.sensors)

	// v1.5 stays off: the previous server only ever spoke pre-session v1.5
	// (Get Channel Auth Capabilities, which the RMCP+ discovery path still
	// answers), so enabling MD5 sessions now would grow the surface, not
	// preserve it.
	b := bmc.New(deviceInfo, bmcGUID, h, bmc.WithV15Disabled())

	user, err := b.Users.Add(2, d.username)
	if err != nil {
		return nil, fmt.Errorf("add ipmi user: %w", err)
	}
	user.SetPassword([]byte(d.password))
	user.Enabled = true
	user.ChannelAccess[1] = bmc.UserChannelAccess{
		MaxPrivilege: bmc.PrivilegeLevelAdministrator,
		Enabled:      true,
	}

	reg := handlers.NewRegistry()
	// Middleware must be installed before handlers are registered — Use is
	// not retroactive.
	reg.Use(telemetryMiddleware(ctx))
	handlers.RegisterAllHandlers(reg)
	registerSensorHandlers(reg, h.sensors)
	registerOEMHandlers(reg, d.firmware)

	if err := seedStorage(ctx, h.storage); err != nil {
		return nil, fmt.Errorf("seed ipmi storage: %w", err)
	}

	conn, err := udp.Listen(fmt.Sprintf(":%d", d.port))
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}

	srv := server.NewServer(b, conn, server.WithHandlerRegistry(reg))

	serveCtx, cancel := context.WithCancel(ctx)
	s := &Server{srv: srv, cancel: cancel, done: make(chan struct{}), addr: conn.LocalAddr()}
	go func() {
		defer close(s.done)
		if err := srv.Serve(serveCtx); err != nil && serveCtx.Err() == nil {
			slog.ErrorContext(serveCtx, "ipmi serve failed", slog.Any("err", err))
		}
	}()

	slog.InfoContext(ctx, "ipmi server started", slog.Any("addr", s.addr))
	return s, nil
}

// Addr returns the bound UDP address (useful when the configured port is 0).
func (s *Server) Addr() net.Addr { return s.addr }

// Stop shuts the server down and waits for the serve loop to exit.
func (s *Server) Stop() {
	s.cancel()
	_ = s.srv.Close()
	<-s.done
	slog.Info("ipmi server stopped")
}

// telemetryMiddleware counts dispatched commands through the existing IPMI
// packet counters. It counts commands rather than raw datagrams (session
// setup and ASF pings are handled below the registry), which is the traffic
// an operator actually cares about on the metrics panel.
func telemetryMiddleware(root context.Context) handlers.Middleware {
	return func(next handlers.Handler) handlers.Handler {
		return handlers.HandlerFunc(func(ctx context.Context, hctx *handlers.HandlerContext, data []byte) ([]byte, types.CompletionCode, error) {
			telemetry.IPMIPacketReceived(root)
			resp, cc, err := next.Handle(ctx, hctx, data)
			telemetry.IPMIPacketSent(root)
			return resp, cc, err
		})
	}
}
