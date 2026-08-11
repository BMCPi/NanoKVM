package vm

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/pkg/video/rtc"
)

// maxSignalMessage bounds one inbound signaling message. An SDP offer with a
// long candidate list runs to a few KB; 64 KiB leaves generous headroom while
// keeping a client from making the BMC allocate without limit on a socket that
// is cheap to open.
const maxSignalMessage = 64 << 10

// wsSignaler adapts a WebSocket connection to rtc.Signaler.
//
// The mutex is required, not defensive: a gorilla connection permits one
// writer at a time, and the session writes from the ICE agent's goroutine, the
// hub's state watcher and the request goroutine concurrently.
type wsSignaler struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (w *wsSignaler) Send(m rtc.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ws.SetWriteDeadline(time.Now().Add(messageWait)); err != nil {
		return err
	}
	return w.ws.WriteJSON(m)
}

// Video upgrades the connection to a WebSocket and runs one WebRTC signaling
// session over it, streaming the host's HDMI output to the browser.
//
// The socket carries signaling only: once negotiation completes the video
// leaves over SRTP directly, so a dropped WebSocket after connection setup
// costs nothing but the ability to renegotiate. Closing it is still what ends
// the session, because it is the only liveness signal that tells the BMC to
// stop encoding for a viewer who has gone away.
func (s *Service) Video(c *gin.Context) {
	if s.VideoHub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "video capture is not available on this device"})
		return
	}

	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("failed to init video websocket: %s", err)
		return
	}
	defer func() {
		_ = ws.Close()
	}()
	ws.SetReadLimit(maxSignalMessage)

	sig := &wsSignaler{ws: ws}

	session, err := s.VideoHub.NewSession(sig)
	if err != nil {
		log.Errorf("failed to start video session: %s", err)
		_ = sig.Send(rtc.Message{Type: rtc.TypeError, Error: err.Error()})
		return
	}
	defer session.Close()

	// The session can end on its own -- ICE failure, or the hub shutting
	// down -- while this goroutine is parked in ReadJSON. Closing the socket
	// is what unblocks it.
	go func() {
		<-session.Done()
		_ = ws.Close()
	}()

	for {
		var m rtc.Message
		if err := ws.ReadJSON(&m); err != nil {
			return
		}
		if err := session.HandleMessage(m); err != nil {
			// Report and keep the socket open: a rejected candidate or a
			// malformed offer is recoverable, and the browser can retry
			// on the same session.
			log.Warnf("video signaling: %s", err)
			_ = sig.Send(rtc.Message{Type: rtc.TypeError, Error: err.Error()})
		}
	}
}
