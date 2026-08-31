package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/hid"
)

// HID input for the HDMI console: keyboard and mouse events from the browser,
// keyboard lock state back.
//
// Input rides a dedicated WebSocket rather than the video peer connection's
// data channel (which is how JetKVM carries its hidrpc). Two reasons: input has
// to work when video is not streaming — macros, blind typing at a BIOS prompt,
// the virtual keyboard with the HDMI tab closed — and the transport then has no
// dependency on WebRTC negotiation succeeding at all. The event messages are
// small JSON objects with short field names; at a few hundred bytes per
// keystroke the framing overhead is irrelevant next to the video stream.

// hidEvent is one client message. The type field selects which of the rest
// apply; unknown types are ignored so a newer browser bundle talking to an
// older server degrades instead of dropping the socket.
type hidEvent struct {
	Type string `json:"t"`

	// Keyboard. Code is a HID usage code, already mapped from the browser's
	// KeyboardEvent.code by the client (pkg/hid.KeyCodes is the same table).
	Code     byte   `json:"c"`
	Modifier byte   `json:"m"`
	Keys     []byte `json:"k"`

	// Mouse. X/Y are absolute in 0..hid.AbsMax; DX/DY are relative deltas.
	X       uint16 `json:"x"`
	Y       uint16 `json:"y"`
	DX      int8   `json:"dx"`
	DY      int8   `json:"dy"`
	Buttons byte   `json:"b"`
	Wheel   int8   `json:"w"`
}

// Client message types.
const (
	evKeyDown   = "kd"    // press one key
	evKeyUp     = "ku"    // release one key
	evKeyReport = "kr"    // assert an explicit report (combo, virtual keyboard)
	evAbsMouse  = "abs"   // absolute pointer move
	evRelMouse  = "rel"   // relative pointer move
	evReset     = "reset" // release everything
)

// hidSocketWriter serialises writes to one client socket. The LED watcher and
// the reader loop both send, and a gorilla connection allows one writer at a
// time.
type hidSocketWriter struct {
	mu sync.Mutex
	ws *websocket.Conn
}

func (w *hidSocketWriter) send(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ws.SetWriteDeadline(time.Now().Add(messageWait)); err != nil {
		return err
	}
	return w.ws.WriteJSON(v)
}

// HID upgrades to a WebSocket and applies the client's keyboard and mouse
// events to the USB HID gadget.
func (s *Service) HID(c *gin.Context) {
	ctrl := s.HIDGadget
	if ctrl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: "HID gadget is not available on this device"})
		return
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("hid: failed to init websocket: %s", err)
		return
	}

	writer := &hidSocketWriter{ws: ws}

	// Whatever this client was holding must not stay held on the host after it
	// goes away. This is the same reason the pane releases on blur, one layer
	// down: a dropped socket is indistinguishable from a lost key-up.
	defer func() {
		if err := ctrl.ReleaseAll(); err != nil {
			log.Debugf("hid: release on disconnect failed: %s", err)
		}
		_ = ws.Close()
	}()

	// Seed the client with the current lock state and held keys, then stream
	// changes. A second browser, or a reload mid-keystroke, otherwise shows
	// indicators that disagree with the host.
	if err := writer.send(gin.H{"t": "leds", "leds": ctrl.LEDs()}); err != nil {
		return
	}
	if err := writer.send(gin.H{"t": "keys", "keys": ctrl.KeysDown()}); err != nil {
		return
	}

	leds, cancelLEDs := ctrl.WatchLEDs()
	defer cancelLEDs()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case state, ok := <-leds:
				if !ok {
					return
				}
				if err := writer.send(gin.H{"t": "leds", "leds": state}); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	log.Debugf("hid: input session from %s opened", c.ClientIP())
	s.readHIDEvents(ws, ctrl)
	log.Debugf("hid: input session from %s closed", c.ClientIP())
}

// keyEventBuffer bounds the ordered keyboard queue. Key events are stateful
// (a lost key-up sticks a key until the auto-release fires), so they are
// dropped only when the host has stalled long enough to fill this — at which
// point there is nobody to type at anyway.
const keyEventBuffer = 64

// hidApplier decouples the socket read loop from device writes. A hidg write
// can take a poll interval — or a 250ms deadline when the host stops
// listening — and applying events inline would queue every later event behind
// it. Worse, browsers emit pointer moves at 60–250Hz, so an inline loop puts
// dozens of mouse reports ahead of each keystroke.
//
// The keyboard and the pointer are separate USB endpoints with no ordering
// requirement between them, so each gets its own lane: key events flow
// through an ordered channel (order between key-down and key-up is
// correctness), pointer moves collapse into a latest-wins slot — a stale
// cursor position has no value once a newer one exists, which caps the mouse
// backlog at one report no matter how fast the browser fires. Wheel ticks and
// relative deltas are accumulated rather than replaced so scrolling and
// pointer-lock movement are not lost to coalescing. A button transition that
// both begins and ends inside one in-flight write (~10ms) can collapse; a
// human click cannot do that.
type hidApplier struct {
	apply func(hidEvent) error
	keys  chan hidEvent
	done  chan struct{}

	mu      sync.Mutex
	pending *hidEvent
	kick    chan struct{}
}

func newHIDApplier(apply func(hidEvent) error) *hidApplier {
	a := &hidApplier{
		apply: apply,
		keys:  make(chan hidEvent, keyEventBuffer),
		kick:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	go a.keyboardLane()
	go a.pointerLane()
	return a
}

func (a *hidApplier) stop() { close(a.done) }

// enqueue routes one client event to its lane. Never blocks.
func (a *hidApplier) enqueue(ev hidEvent) {
	switch ev.Type {
	case evAbsMouse, evRelMouse:
		a.mu.Lock()
		a.coalesceLocked(ev)
		a.mu.Unlock()
		select {
		case a.kick <- struct{}{}:
		default:
		}
		return
	case evReset:
		// A reset supersedes any pointer state still waiting to be written.
		a.mu.Lock()
		a.pending = nil
		a.mu.Unlock()
	}

	select {
	case a.keys <- ev:
	case <-a.done:
	default:
		log.Debugf("hid: keyboard queue full; dropping %q (host not consuming reports)", ev.Type)
	}
}

// coalesceLocked folds a pointer event into the pending slot.
func (a *hidApplier) coalesceLocked(ev hidEvent) {
	p := a.pending
	if p == nil || p.Type != ev.Type {
		a.pending = &ev
		return
	}
	if ev.Type == evRelMouse {
		// Deltas are movement; summing preserves it through coalescing.
		ev.DX = clampInt8(int(p.DX) + int(ev.DX))
		ev.DY = clampInt8(int(p.DY) + int(ev.DY))
	}
	// Wheel ticks accumulate in both modes — each tick is a scroll notch the
	// host should still see. Position and buttons take the newest value.
	ev.Wheel = clampInt8(int(p.Wheel) + int(ev.Wheel))
	a.pending = &ev
}

func clampInt8(v int) int8 {
	if v > 127 {
		return 127
	}
	if v < -128 {
		return -128
	}
	return int8(v)
}

// keyboardLane preserves strict event order for the stateful keyboard report.
func (a *hidApplier) keyboardLane() {
	for {
		select {
		case <-a.done:
			return
		case ev := <-a.keys:
			if err := a.apply(ev); err != nil {
				// Usually "the host is not listening", which is normal for a
				// BMC and already logged at debug by pkg/hid.
				log.Debugf("hid: applying %q failed: %s", ev.Type, err)
			}
		}
	}
}

// pointerLane drains the latest-wins slot. The inner loop re-checks after each
// write so a position that arrived mid-write goes out immediately rather than
// waiting for another kick.
func (a *hidApplier) pointerLane() {
	for {
		select {
		case <-a.done:
			return
		case <-a.kick:
		}
		for {
			a.mu.Lock()
			ev := a.pending
			a.pending = nil
			a.mu.Unlock()
			if ev == nil {
				break
			}
			if err := a.apply(*ev); err != nil {
				log.Debugf("hid: applying %q failed: %s", ev.Type, err)
			}
		}
	}
}

// readHIDEvents is the client read loop. A malformed message is skipped rather
// than fatal: dropping a whole input session over one bad frame would lose the
// operator's keyboard mid-sentence. Events are enqueued, never applied inline:
// the read loop must keep consuming even while a device write stalls.
func (s *Service) readHIDEvents(ws *websocket.Conn, ctrl *hid.Controller) {
	applier := newHIDApplier(func(ev hidEvent) error { return applyHIDEvent(ctrl, ev) })
	defer applier.stop()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}

		var ev hidEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Debugf("hid: bad event from client: %s", err)
			continue
		}

		applier.enqueue(ev)
	}
}

func applyHIDEvent(ctrl *hid.Controller, ev hidEvent) error {
	switch ev.Type {
	case evKeyDown:
		return ctrl.Key(ev.Code, true)
	case evKeyUp:
		return ctrl.Key(ev.Code, false)
	case evKeyReport:
		return ctrl.KeyReport(ev.Modifier, ev.Keys)
	case evAbsMouse:
		return ctrl.AbsMouse(ev.X, ev.Y, ev.Buttons, ev.Wheel)
	case evRelMouse:
		return ctrl.RelMouse(ev.DX, ev.DY, ev.Buttons, ev.Wheel)
	case evReset:
		return ctrl.ReleaseAll()
	default:
		return nil
	}
}

// ── Macros ────────────────────────────────────────────────────────────────

// GetMacros lists the stored macros in display order.
func (s *Service) GetMacros(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"macros": sortedMacros(s.Conf)})
}

// CreateMacro appends a macro. The ID is assigned here so a client cannot
// collide with an existing one.
func (s *Service) CreateMacro(c *gin.Context) {
	var macro config.KeyboardMacro
	if err := c.ShouldBindJSON(&macro); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: err.Error()})
		return
	}

	conf := s.Conf
	if len(conf.Macros) >= config.MaxMacros {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: "the macro limit has been reached"})
		return
	}

	macro.ID = uuid.NewString()
	macro.SortOrder = len(conf.Macros)
	if err := validateMacro(&macro); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: err.Error()})
		return
	}

	conf.Macros = append(conf.Macros, macro)
	config.Save()
	log.Infof("hid: macro %q created", macro.Name)
	c.JSON(http.StatusOK, gin.H{"macro": macro})
}

// UpdateMacro replaces a macro in place, keeping its ID.
func (s *Service) UpdateMacro(c *gin.Context) {
	var macro config.KeyboardMacro
	if err := c.ShouldBindJSON(&macro); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: err.Error()})
		return
	}

	id := c.Param("id")
	conf := s.Conf
	idx := macroIndex(conf, id)
	if idx < 0 {
		c.JSON(http.StatusNotFound, gin.H{errorKey: "no such macro"})
		return
	}

	macro.ID = id
	if err := validateMacro(&macro); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: err.Error()})
		return
	}

	conf.Macros[idx] = macro
	config.Save()
	log.Infof("hid: macro %q updated", macro.Name)
	c.JSON(http.StatusOK, gin.H{"macro": macro})
}

// DeleteMacro removes a macro.
func (s *Service) DeleteMacro(c *gin.Context) {
	conf := s.Conf
	idx := macroIndex(conf, c.Param("id"))
	if idx < 0 {
		c.JSON(http.StatusNotFound, gin.H{errorKey: "no such macro"})
		return
	}

	name := conf.Macros[idx].Name
	conf.Macros = append(conf.Macros[:idx], conf.Macros[idx+1:]...)
	config.Save()
	log.Infof("hid: macro %q deleted", name)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RunMacro plays a stored macro at the host.
func (s *Service) RunMacro(c *gin.Context) {
	ctrl := s.HIDGadget
	if ctrl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: "HID gadget is not available on this device"})
		return
	}

	conf := s.Conf
	idx := macroIndex(conf, c.Param("id"))
	if idx < 0 {
		c.JSON(http.StatusNotFound, gin.H{errorKey: "no such macro"})
		return
	}
	macro := conf.Macros[idx]

	steps, err := resolveMacro(macro)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: err.Error()})
		return
	}

	// Bounded independently of the request: a macro is at most
	// MaxStepsPerMacro steps of at most MaxStepDelayMS each, and the operator
	// should not be able to hold a handler open longer than that.
	ctx, cancel := context.WithTimeout(c.Request.Context(),
		time.Duration(config.MaxStepsPerMacro)*(time.Duration(config.MaxStepDelayMS)*time.Millisecond)+5*time.Second)
	defer cancel()

	if err := ctrl.RunMacro(ctx, steps); err != nil {
		log.Warnf("hid: macro %q failed: %s", macro.Name, err)
		c.JSON(http.StatusInternalServerError, gin.H{errorKey: err.Error()})
		return
	}

	log.Infof("hid: macro %q sent", macro.Name)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// validateMacro applies the config-level rules, then resolves every key name so
// a macro that cannot run is rejected at the point it is saved rather than the
// first time the operator reaches for it.
func validateMacro(macro *config.KeyboardMacro) error {
	if err := macro.Validate(); err != nil {
		return err
	}
	_, err := resolveMacro(*macro)
	return err
}

func resolveMacro(macro config.KeyboardMacro) ([]hid.Step, error) {
	steps := make([]hid.Step, 0, len(macro.Steps))
	for i, s := range macro.Steps {
		step, err := hid.ResolveStep(s.Keys, s.Modifiers, s.Delay)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func macroIndex(conf *config.Config, id string) int {
	if id == "" {
		return -1
	}
	for i := range conf.Macros {
		if conf.Macros[i].ID == id {
			return i
		}
	}
	return -1
}

// sortedMacros returns the macros in display order, leaving the stored slice
// untouched.
func sortedMacros(conf *config.Config) []config.KeyboardMacro {
	out := append([]config.KeyboardMacro(nil), conf.Macros...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out
}
