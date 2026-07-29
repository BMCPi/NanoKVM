package vm

import (
	"encoding/json"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/pi-bmc/nanokvm-app/server/service/shell"
)

// Shell upgrades the HTTP connection to a WebSocket and bridges it to an
// interactive shell running on the BMC itself, on its own pseudo-terminal.
//
// This is the counterpart to Terminal: that one attaches to the target host's
// serial port, this one is a local shell. Each connection gets its own PTY and
// its own shell process; closing the socket kills the session. The session
// plumbing is shared with the SSH server (server/service/shell).
//
// Wire protocol matches Terminal so the same xterm.js client code works: text
// frames are keystrokes, binary frames are JSON WinSize resizes, and
// everything the shell prints comes back as binary frames.
func (s *Service) Shell(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorf("failed to init websocket: %s", err)
		return
	}
	defer func() {
		_ = ws.Close()
	}()

	session, err := shell.Start(shell.Options{})
	if err != nil {
		log.Errorf("failed to start bmc shell: %s", err)
		_ = ws.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
		return
	}
	defer session.Close()

	log.Infof("bmc shell session started (pid %d)", session.Pid())

	// Shell → WebSocket. Closing the socket here unblocks the read loop below
	// when the shell exits on its own (e.g. the user typed `exit`).
	go func() {
		_, _ = io.Copy(&wsWriter{ws: ws}, session)
		_ = ws.Close()
	}()

	// WebSocket → shell. No read deadline: a session can idle for hours.
	var zeroTime time.Time
	_ = ws.SetReadDeadline(zeroTime)

	for {
		msgType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		if msgType == websocket.BinaryMessage {
			var winSize WinSize
			if json.Unmarshal(p, &winSize) == nil && winSize.Cols > 0 && winSize.Rows > 0 {
				session.Resize(winSize.Cols, winSize.Rows)
			}
			continue
		}

		if _, err := session.Write(p); err != nil {
			return
		}
	}
}
