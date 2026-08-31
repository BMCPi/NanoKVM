package fragments

// fragments_power_events.go streams the navbar power pill and the dropdown's
// power toggle as rendered HTML fragments over SSE, so htmx can swap them
// directly instead of a hand-rolled script re-deriving the toggle's hx-post
// path, label and icon visibility from a JSON payload (see
// docs/superpowers/plans/2026-08-30-htmx-sse-power.md — that duplication is
// what let the toggle drift and post the previous action after a
// transition). /api/vm/gpio/events keeps its JSON contract for its existing
// consumers; this endpoint is additive.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/deps"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
	"github.com/pi-bmc/nanokvm-app/ui/components"
)

const (
	// Same constraints as api/vm.sseHeartbeat / legacyPollInterval: this
	// stream reads the same controller, so the same proxy-buffering and
	// legacy-polling reasoning applies.
	powerEventsHeartbeat  = 30 * time.Second
	powerEventsLegacyPoll = 5 * time.Second
)

// powerEventFrame renders one SSE frame.
//
// SSE is line-oriented: a `data:` line ends at the first \n, so a raw
// newline inside a rendered templ fragment truncates the event there and the
// client swaps a partial fragment. Collapsing newlines to spaces is safe for
// this payload because it is markup, where the newline between tags is
// insignificant whitespace, not content the browser renders.
func powerEventFrame(name, html string) string {
	single := strings.NewReplacer("\r\n", " ", "\n", " ").Replace(html)
	return "event: " + name + "\ndata: " + single + "\n\n"
}

// renderPowerComponent renders a templ component to a string. powerEventFrame
// needs the whole payload up front to collapse its newlines, but
// templ.Component only knows how to write to an io.Writer.
func renderPowerComponent(ctx context.Context, c templ.Component) (string, error) {
	var sb strings.Builder
	if err := c.Render(ctx, &sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// writePowerEvents renders the pill and the toggle for the given state and
// writes both as SSE frames, in that order. They always travel together:
// both are derived from the same boolean, and sending only one would let a
// client that missed a frame show a pill and a toggle that disagree.
func writePowerEvents(ctx context.Context, w io.Writer, on bool) error {
	pill, err := renderPowerComponent(ctx, components.PowerPill(on, true))
	if err != nil {
		return err
	}
	toggle, err := renderPowerComponent(ctx, components.PowerToggleBtn(on))
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, powerEventFrame("powerpill", pill)); err != nil {
		return err
	}
	_, err = io.WriteString(w, powerEventFrame("powertoggle", toggle))
	return err
}

// getPowerEvents streams the navbar power pill and toggle as SSE HTML
// fragments.
//
// It follows api/vm.StreamGpio's shape exactly (initial write, edge-watch
// with a legacy poll fallback, heartbeat) because it answers from the same
// controller and inherits the same failure modes — including the reason a
// stuck state must refuse with a real error status rather than a 200
// carrying something other than text/event-stream: EventSource treats that
// as fatal and never reconnects on its own.
func getPowerEvents(d *deps.Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctrl := d.Power

		// Subscribe before reading the initial state: a transition landing
		// between the two is then queued on changes rather than lost.
		changes, cancel, err := ctrl.Watch()
		if err != nil && !errors.Is(err, power.ErrNoEdgeEvents) {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		if cancel != nil {
			defer cancel()
		}

		// Refuse before any SSE headers go out, same as streamUnavailable in
		// api/vm/gpio.go: a momentarily unreadable power LED must look like a
		// failed request, not an empty-but-healthy stream.
		pwr, err := ctrl.State(c.Request.Context())
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}

		// Legacy mode: synthesise a change feed by polling.
		var poll <-chan time.Time
		if changes == nil {
			ticker := time.NewTicker(powerEventsLegacyPoll)
			defer ticker.Stop()
			poll = ticker.C
		}

		heartbeat := time.NewTicker(powerEventsHeartbeat)
		defer heartbeat.Stop()

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		// Defeat proxy response buffering, which would hold events until the
		// (never-ending) stream closed.
		c.Header("X-Accel-Buffering", "no")

		ctx := c.Request.Context()

		if err := writePowerEvents(ctx, c.Writer, pwr); err != nil {
			return
		}
		c.Writer.Flush()

		for {
			select {
			case <-ctx.Done():
				return

			case on, ok := <-changes:
				if !ok {
					return
				}
				pwr = on
				if err := writePowerEvents(ctx, c.Writer, on); err != nil {
					return
				}
				c.Writer.Flush()

			case <-poll:
				on, err := ctrl.State(ctx)
				if err != nil {
					log.Debugf("power events stream: poll failed: %s", err)
					continue
				}
				if on == pwr {
					continue
				}
				pwr = on
				if err := writePowerEvents(ctx, c.Writer, on); err != nil {
					return
				}
				c.Writer.Flush()

			case <-heartbeat.C:
				// A comment line: keeps proxies from reaping an idle
				// connection, and EventSource ignores it.
				if _, err := io.WriteString(c.Writer, ": ping\n\n"); err != nil {
					return
				}
				c.Writer.Flush()
			}
		}
	}
}
