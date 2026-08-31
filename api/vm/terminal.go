package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/pi-bmc/nanokvm-app/pkg/serial"
)

const (
	messageWait    = 10 * time.Second
	maxMessageSize = 1024
)

// WinSize is sent by the xterm.js client on resize. Logged but not acted
// upon because the serial port has no concept of terminal dimensions.
type WinSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  maxMessageSize,
	WriteBufferSize: maxMessageSize,
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

// wsWriter adapts a WebSocket connection to io.Writer so the serial
// broker can fan out port output to the client.
type wsWriter struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ws.SetWriteDeadline(time.Now().Add(messageWait)); err != nil {
		return 0, err
	}
	if err := w.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Terminal upgrades the HTTP connection to a WebSocket and bridges it to
// the shared serial port via the serial broker.
func (h *handlers) Terminal(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "failed to init websocket", slog.Any("err", err))
		return
	}
	defer func() {
		_ = ws.Close()
	}()

	sessionID := fmt.Sprintf("ws-%s-%d", c.ClientIP(), time.Now().UnixNano())
	broker := serial.GetBroker()

	writer := &wsWriter{ws: ws}
	_, err = broker.Connect(sessionID, writer)
	if err != nil {
		h.log.ErrorContext(c.Request.Context(), "serial broker connect failed", slog.Any("err", err))
		// Best-effort error message to the client before closing.
		_ = ws.WriteMessage(websocket.TextMessage, []byte("serial error: "+err.Error()))
		return
	}
	defer broker.Disconnect(sessionID)

	// Unblock the read loop below at shutdown: ws.ReadMessage() has no read
	// deadline (see below) and would otherwise block forever, so the deferred
	// broker.Disconnect above would never run and the serial port would stay
	// held open through process teardown. The watcher runs on a ctx derived
	// from h.d.Ctx, not h.d.Ctx directly, so it also exits promptly on normal
	// handler return instead of leaking for the life of the process.
	shutdownCtx, cancelShutdownWatch := context.WithCancel(h.d.Ctx)
	defer cancelShutdownWatch()
	go func() {
		<-shutdownCtx.Done()
		_ = ws.Close()
	}()

	// Read loop: forward WebSocket messages to the serial port.
	var zeroTime time.Time
	_ = ws.SetReadDeadline(zeroTime)

	for {
		msgType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		// Binary messages from xterm.js carry resize notifications.
		if msgType == websocket.BinaryMessage {
			var winSize WinSize
			if json.Unmarshal(p, &winSize) == nil {
				h.log.DebugContext(c.Request.Context(), "terminal resize (ignored – serial)",
					slog.Int("cols", int(winSize.Cols)), slog.Int("rows", int(winSize.Rows)))
			}
			continue
		}

		// Text messages are keyboard input destined for the serial port.
		if _, err := broker.Write(p); err != nil {
			h.log.ErrorContext(c.Request.Context(), "serial write failed", slog.Any("err", err))
			return
		}
	}
}

// TerminalCapture serves the persisted host serial capture (rotated
// generation first, then current) as plain text — the managed host's boot
// and crash logs, recorded by the always-on capture even when no live
// console session was attached.
func (h *handlers) TerminalCapture(c *gin.Context) {
	files := serial.CaptureFiles()
	if len(files) == 0 {
		c.String(http.StatusNotFound, "no serial capture available")
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Status(http.StatusOK)
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_, _ = io.Copy(c.Writer, f)
		_ = f.Close()
	}
}
