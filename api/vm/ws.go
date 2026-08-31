package vm

import (
	"context"

	"github.com/gorilla/websocket"
)

// closeOnShutdown starts a goroutine that closes ws once parent is done, and
// returns a release func the caller must defer to stop watching.
//
// http.Server.Shutdown has no visibility into a hijacked connection -- a
// WebSocket upgrade takes it out of net/http's bookkeeping entirely -- so it
// can neither wait for one nor interrupt it. Every handler in this package
// then blocks in ws.ReadMessage() with no read deadline (sessions are meant
// to idle for hours), so without this a shutdown would hang behind that read
// forever, and none of the deferred cleanup below it -- closing a shell
// session, disconnecting the serial broker, stopping the HID applier -- would
// ever run. Closing the socket makes ReadMessage return an error, so the read
// loop exits and its deferred cleanup fires.
//
// The watcher runs on a context derived from parent, not parent directly, so
// it also exits promptly when the handler returns normally instead of
// leaking a goroutine for the life of the process. Typical use:
//
//	release := closeOnShutdown(h.d.Ctx, ws)
//	defer release()
func closeOnShutdown(parent context.Context, ws *websocket.Conn) (release func()) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		<-ctx.Done()
		_ = ws.Close()
	}()
	return cancel
}
