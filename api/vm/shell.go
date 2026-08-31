package vm

import (
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/pi-bmc/nanokvm-app/pkg/shell"
)

// Shell upgrades the HTTP connection to a WebSocket and bridges it to an
// interactive shell running on the BMC itself, on its own pseudo-terminal.
//
// This is the counterpart to Terminal: that one attaches to the target host's
// serial port, this one is a local shell. Each connection gets its own PTY and
// its own shell process; closing the socket kills the session. The session
// plumbing is shared with the SSH server (pkg/shell).
//
// Wire protocol matches Terminal so the same xterm.js client code works: text
// frames are keystrokes, binary frames are JSON WinSize resizes, and
// everything the shell prints comes back as binary frames.
func (s *Service) Shell(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "failed to init websocket", slog.Any("err", err))
		return
	}
	defer func() {
		_ = ws.Close()
	}()

	session, err := shell.Start(shell.Options{})
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "failed to start bmc shell", slog.Any("err", err))
		_ = ws.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
		return
	}
	defer session.Close()

	slog.InfoContext(c.Request.Context(), "bmc shell session started", slog.Int("pid", session.Pid()))

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
