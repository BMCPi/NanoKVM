package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/pkg/proto"
)

const (
	// sseHeartbeat bounds how long an idle stream stays silent. Proxies reap
	// connections with no traffic; a comment line keeps them open without
	// reaching the client's message handler.
	sseHeartbeat = 30 * time.Second

	// legacyPollInterval is the fallback cadence when the controller has no
	// power-LED line to watch and so cannot deliver edge events.
	legacyPollInterval = 5 * time.Second
)

func (s *Service) SetGpio(c *gin.Context) {
	var req proto.SetGpioReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, &req); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("invalid arguments: %s", err))
		return
	}

	ctrl := s.Power
	var err error

	// Not the request context: a power action is a command, and abandoning one
	// midway is worse than finishing it. Reset in particular is off-then-on, so
	// a client that hangs up between the two phases would leave the host down.
	// This context still dies at shutdown. See deps.ActionContext.
	ctx, cancel := s.Deps.ActionContext(power.ActionTimeout)
	defer cancel()

	switch req.Action {
	case "on":
		err = ctrl.PowerOn(ctx)
	case "off":
		err = ctrl.PowerOff(ctx)
	case "forceoff":
		err = ctrl.ForceOff(ctx)
	case "reset":
		err = ctrl.Reset(ctx)
	default:
		rsp.ErrRsp(c, -2, fmt.Sprintf("invalid action: %s", req.Action))
		return
	}

	if err != nil {
		rsp.ErrRsp(c, -3, fmt.Sprintf("operation failed: %s", err))
		return
	}

	log.Debugf("power action %s completed", req.Action)
	rsp.OkRsp(c)
}

func (s *Service) GetGpio(c *gin.Context) {
	var rsp proto.Response

	ctrl := s.Power
	pwr, err := ctrl.State(c.Request.Context())
	if err != nil {
		rsp.ErrRsp(c, -2, fmt.Sprintf("failed to read power state: %s", err))
		return
	}

	data := &proto.GetGpioRsp{
		PWR: pwr,
	}
	rsp.OkRspWithData(c, data)
}

// StreamGpio pushes power-state changes to the client as Server-Sent Events.
//
// The stream opens with the current state, then emits one `power` event per
// transition — driven by GPIO edge events, so a change reaches the browser as
// fast as the kernel reports it. In legacy mode the controller has no LED line
// to watch, so the stream degrades to polling State on a ticker; the wire format
// is identical either way and the client cannot tell the difference.
//
// Each event carries the same JSON body as GET /api/vm/gpio's data field:
//
//	event: power
//	data: {"pwr":true,"hdd":false}
func (s *Service) StreamGpio(c *gin.Context) {
	ctrl := s.Power

	// Subscribe before reading the initial state: a transition landing between
	// the two is then queued on changes rather than lost. The client may see the
	// same value twice, which is harmless.
	changes, cancel, err := ctrl.Watch()
	if err != nil && !errors.Is(err, power.ErrNoEdgeEvents) {
		var rsp proto.Response
		rsp.ErrRsp(c, -3, fmt.Sprintf("failed to watch power state: %s", err))
		return
	}
	if cancel != nil {
		defer cancel()
	}

	// Report an unreadable LED as a plain error before any SSE headers go out,
	// so the client sees a failed request rather than an empty stream that
	// EventSource would silently retry forever.
	pwr, err := ctrl.State(c.Request.Context())
	if err != nil {
		var rsp proto.Response
		rsp.ErrRsp(c, -2, fmt.Sprintf("failed to read power state: %s", err))
		return
	}

	// Legacy mode: synthesise a change feed by polling.
	var poll <-chan time.Time
	if changes == nil {
		ticker := time.NewTicker(legacyPollInterval)
		defer ticker.Stop()
		poll = ticker.C
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	// Defeat proxy response buffering, which would hold events until the
	// (never-ending) stream closed.
	c.Header("X-Accel-Buffering", "no")

	writePower(c.Writer, pwr)
	c.Writer.Flush()

	ctx := c.Request.Context()
	for {
		select {
		case <-ctx.Done():
			return

		case on, ok := <-changes:
			if !ok {
				return
			}
			writePower(c.Writer, on)
			c.Writer.Flush()

		case <-poll:
			on, err := ctrl.State(ctx)
			if err != nil {
				log.Debugf("gpio stream: poll failed: %s", err)
				continue
			}
			if on == pwr {
				continue
			}
			pwr = on
			writePower(c.Writer, on)
			c.Writer.Flush()

		case <-heartbeat.C:
			// A comment line: keeps proxies from reaping an idle connection,
			// and EventSource ignores it.
			if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// writePower emits one SSE `power` event carrying a GetGpioRsp body.
func writePower(w io.Writer, on bool) {
	body, err := json.Marshal(proto.GetGpioRsp{PWR: on})
	if err != nil {
		return // GetGpioRsp is two bools; unreachable.
	}
	if _, err := fmt.Fprintf(w, "event: power\ndata: %s\n\n", body); err != nil {
		log.Debugf("gpio stream: write failed: %s", err)
	}
}
