package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

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

func (h *handlers) SetGpio(c *gin.Context) {
	var req proto.SetGpioReq
	var rsp proto.Response

	if err := proto.ParseFormRequest(c, h.log, &req); err != nil {
		rsp.ErrRsp(c, -1, fmt.Sprintf("invalid arguments: %s", err))
		return
	}

	ctrl := h.d.Power
	var err error

	// Not the request context: a power action is a command, and abandoning one
	// midway is worse than finishing it. Reset in particular is off-then-on, so
	// a client that hangs up between the two phases would leave the host down.
	// This context still dies at shutdown. See deps.ActionContext.
	ctx, cancel := h.d.ActionContext(power.ActionTimeout)
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

	h.log.DebugContext(c.Request.Context(), "power action completed", slog.String("action", req.Action))
	rsp.OkRsp(c)
}

func (h *handlers) GetGpio(c *gin.Context) {
	var rsp proto.Response

	ctrl := h.d.Power
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
func (h *handlers) StreamGpio(c *gin.Context) {
	ctrl := h.d.Power

	// Subscribe before reading the initial state: a transition landing between
	// the two is then queued on changes rather than lost. The client may see the
	// same value twice, which is harmless.
	changes, cancel, err := ctrl.Watch()
	if err != nil && !errors.Is(err, power.ErrNoEdgeEvents) {
		streamUnavailable(c, -3, fmt.Sprintf("failed to watch power state: %s", err))
		return
	}
	if cancel != nil {
		defer cancel()
	}

	// Report an unreadable LED before any SSE headers go out, so the client
	// sees a failed request rather than an empty stream it cannot distinguish
	// from a healthy but quiet one.
	pwr, err := ctrl.State(c.Request.Context())
	if err != nil {
		streamUnavailable(c, -2, fmt.Sprintf("failed to read power state: %s", err))
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

	h.writePower(c.Writer, pwr)
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
			h.writePower(c.Writer, on)
			c.Writer.Flush()

		case <-poll:
			on, err := ctrl.State(ctx)
			if err != nil {
				h.log.DebugContext(ctx, "gpio stream: poll failed", slog.Any("err", err))
				continue
			}
			if on == pwr {
				continue
			}
			pwr = on
			h.writePower(c.Writer, on)
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

// streamUnavailable refuses a stream the controller cannot feed.
//
// The status is what matters, not the body: EventSource treats any response
// whose status is not 200 as a transport failure and reconnects on its own
// schedule, but a 200 carrying something other than text/event-stream is a
// FATAL error — it closes the stream for good and fires no further retries.
// proto.Response.ErrRsp answers 200 with a JSON envelope, which is exactly
// that fatal shape, so this endpoint cannot use it: a momentarily unreadable
// power LED would take the client's stream down until the page was reloaded.
func streamUnavailable(c *gin.Context, code int, msg string) {
	var rsp proto.Response
	rsp.Err(code, msg)
	c.JSON(http.StatusServiceUnavailable, &rsp)
}

// writePower emits one SSE `power` event carrying a GetGpioRsp body.
func (h *handlers) writePower(w io.Writer, on bool) {
	body, err := json.Marshal(proto.GetGpioRsp{PWR: on})
	if err != nil {
		return // GetGpioRsp is two bools; unreachable.
	}
	if _, err := fmt.Fprintf(w, "event: power\ndata: %s\n\n", body); err != nil {
		h.log.Debug("gpio stream: write failed", slog.Any("err", err))
	}
}
