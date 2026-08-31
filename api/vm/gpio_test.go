package vm

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/warthog618/go-gpiosim"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/power"
)

const ledOffset = 4

// readPowerEvent consumes one `event: power` / `data: {...}` pair from an SSE
// stream, skipping heartbeat comments, and returns the pwr field.
func readPowerEvent(t *testing.T, sc *bufio.Scanner) bool {
	t.Helper()

	var sawEvent bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "event: power":
			sawEvent = true
		case strings.HasPrefix(line, "data: "):
			if !sawEvent {
				t.Fatalf("data line without a preceding event line: %q", line)
			}
			var body struct {
				PWR bool `json:"pwr"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &body); err != nil {
				t.Fatalf("decode SSE data %q: %v", line, err)
			}
			return body.PWR
		}
	}
	t.Fatalf("stream ended before a power event arrived: %v", sc.Err())
	return false
}

// TestStreamGpioPushesEdges drives a simulated power-LED line and asserts the
// SSE endpoint emits the initial state and then one event per transition, in the
// wire format EventSource expects.
func TestStreamGpioPushesEdges(t *testing.T) {
	sim, err := gpiosim.NewSimpleton(8)
	if err != nil {
		t.Skipf("gpio-sim unavailable (needs root + gpio-sim module): %v", err)
	}
	t.Cleanup(sim.Close)

	if err := sim.SetPull(ledOffset, 0); err != nil {
		t.Fatalf("drive LED low: %v", err)
	}

	cfg := config.GetInstance()
	cfg.Power.LegacyMode = false
	cfg.Hardware.GPIOPowerLED = config.GPIOPin{Chip: sim.ChipName(), Line: ledOffset}
	ctrl := power.NewController(cfg.Hardware, cfg.Power, slog.New(slog.DiscardHandler))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/vm/gpio/events", (&Service{Power: ctrl}).StreamGpio)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/vm/gpio/events")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	sc := bufio.NewScanner(resp.Body)

	// The stream opens with the current state.
	if on := readPowerEvent(t, sc); on {
		t.Fatal("initial event = on, want off")
	}

	// Each transition produces exactly one event, pushed without polling.
	for _, want := range []bool{true, false, true} {
		level := 0
		if want {
			level = 1
		}
		// Let the handler settle on its select before the edge lands.
		time.Sleep(20 * time.Millisecond)
		if err := sim.SetPull(ledOffset, level); err != nil {
			t.Fatalf("drive LED to %d: %v", level, err)
		}
		if got := readPowerEvent(t, sc); got != want {
			t.Fatalf("event = %v, want %v", got, want)
		}
	}
}

// TestStreamGpioUnreadableStateIsNotOK covers the failure that froze the
// navbar power pill: when the controller cannot report power state the
// handler used to answer 200 with an application/json envelope, and
// EventSource treats a 200 whose Content-Type is not text/event-stream as a
// FATAL error — it closes the stream and never reconnects, so the pill stayed
// stale until the page was reloaded. The status has to be a real error so the
// browser reconnects instead.
func TestStreamGpioUnreadableStateIsNotOK(t *testing.T) {
	cfg := config.GetInstance()
	cfg.Power.LegacyMode = false
	cfg.Hardware.GPIOPowerLED = config.GPIOPin{}
	ctrl := power.NewController(cfg.Hardware, cfg.Power, slog.New(slog.DiscardHandler))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/vm/gpio/events", (&Service{Power: ctrl}).StreamGpio)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/vm/gpio/events")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode < 500 {
		t.Fatalf("status = %d, want a 5xx so EventSource retries instead of closing for good", resp.StatusCode)
	}
}
