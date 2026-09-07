package pages

import "github.com/pi-bmc/nanokvm-app/ui/components"

// HomeModel is the dashboard's server-rendered state.
//
// It exists so the page renders in its final shape on the first paint: which
// console tab is in front is persisted device configuration, and deciding it
// in the browser would mean shipping the serial view and then swapping it,
// which flashes on every load.
type HomeModel struct {
	// HDMIPrimary opens the dashboard on the HDMI tab instead of the serial
	// console. Mirrors config.Console.HDMIPrimary().
	HDMIPrimary bool

	// ICEServersJSON is the marshalled RTCIceServer[] the browser passes to
	// RTCPeerConnection. Empty is spelled "[]", never "".
	ICEServersJSON string

	// Console is what the serial pane's header says about the port the
	// terminal is on, resolved the way the broker resolves it at open time.
	Console components.ConsoleModel
}
