package components

// console_panel_test.go guards the serial pane's layout contract, which
// mirrors the HDMI pane's (see video_panel_test.go): a stage that centers a
// framed console box with margin around it, the chrome inside that frame, and
// an inner mat of padding placed where xterm's fit addon cannot be fooled by
// it. It also pins the header's two badges and the device label's honesty.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
)

func uartSerial() config.Serial {
	return config.Serial{BaudRate: 115200, DataBits: 8, Parity: "none", StopBits: 1}
}

func renderConsolePanel(t *testing.T, m ConsoleModel) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return ConsolePanel(m).Render(context.Background(), w)
	})
}

// openingTag returns the opening tag of the element carrying id.
func openingTag(t *testing.T, html, id string) string {
	t.Helper()
	m := regexp.MustCompile(`<[a-z]+[^>]*\sid="` + regexp.QuoteMeta(id) + `"[^>]*>`).FindString(html)
	if m == "" {
		t.Fatalf("no element with id %q\ngot: %s", id, html)
	}
	return m
}

// The stage owns the tab's space and centers the frame in it; the frame wears
// the video frame's chrome and is capped in width so the centering shows.
func TestConsoleStageCentersAFramedBox(t *testing.T) {
	html := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))

	stage := openingTag(t, html, "console-stage")
	for _, want := range []string{"items-center", "justify-center", "p-4"} {
		if !strings.Contains(stage, want) {
			t.Errorf("stage lacks %s\ngot: %s", want, stage)
		}
	}
	frame := openingTag(t, html, "console-frame")
	for _, want := range []string{"rounded-lg", "bg-black", "ring-1", "max-w-"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame lacks %s (the video frame's chrome)\ngot: %s", want, frame)
		}
	}
}

// Header and terminal both live inside the frame, so the frame is the whole
// console and not a mat with a bar floating above it.
func TestConsoleChromeIsInsideTheFrame(t *testing.T) {
	html := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))

	frame := strings.Index(html, `id="console-frame"`)
	if frame < 0 {
		t.Fatal("no #console-frame")
	}
	for _, id := range []string{`id="btn-serial-connect"`, `id="conn-status-connected"`, `id="terminal-wrap"`, `id="terminal"`} {
		if i := strings.Index(html, id); i < frame {
			t.Errorf("%s is not inside the frame (at %d, frame at %d)", id, i, frame)
		}
	}
}

// xterm's fit addon sizes the grid from the terminal's parent box and
// subtracts only the terminal's own padding, so padding on that parent would
// be counted as room for rows that the box then clips. The inner mat lives one
// level up, on #terminal-wrap, and #terminal-box — the element xterm measures
// — carries none and holds nothing but the terminal.
func TestTerminalParentCarriesNoPadding(t *testing.T) {
	html := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))

	wrap := openingTag(t, html, "terminal-wrap")
	if !regexp.MustCompile(`\bp-[0-9]`).MatchString(wrap) {
		t.Errorf("terminal wrap has no inner padding\ngot: %s", wrap)
	}
	box := openingTag(t, html, "terminal-box")
	if regexp.MustCompile(`\bp[xytrbl]?-[0-9]`).MatchString(box) {
		t.Errorf("the terminal's parent carries padding the fit addon cannot see\ngot: %s", box)
	}
	boxEnd := strings.Index(html, box) + len(box)
	term := strings.Index(html, `id="terminal"`)
	if term < boxEnd {
		t.Fatal("#terminal is not after #terminal-box")
	}
	if between := strings.TrimSpace(html[boxEnd:term]); between != "<div" {
		t.Errorf("something sits between #terminal-box and #terminal: %q", between)
	}
}

// The console name and each connection state are badge components, so their
// typography and sizing come from the same place as every other label here.
func TestConsoleNameAndStatusAreBadges(t *testing.T) {
	html := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))

	named := false
	for _, loc := range regexp.MustCompile(`<span[^>]*data-slot="badge"[^>]*>`).FindAllStringIndex(html, -1) {
		rest := html[loc[1]:]
		if end := strings.Index(rest, "</span>"); end > 0 && strings.Contains(rest[:end], "Serial Console") {
			named = true
		}
	}
	if !named {
		t.Error("the console name is not rendered as a badge")
	}
	for _, state := range []string{"disconnected", "connecting", "connected"} {
		tag := openingTag(t, html, "conn-status-"+state)
		if !strings.Contains(tag, `data-slot="badge"`) {
			t.Errorf("%s status is not a badge\ngot: %s", state, tag)
		}
	}
}

// The label names the port the broker actually opened, with the UART framing
// only when there is a UART to frame.
func TestConsoleDeviceLabelSaysWhichPort(t *testing.T) {
	uart := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))
	for _, want := range []string{"/dev/ttyS1", "115200 8N1"} {
		if !strings.Contains(uart, want) {
			t.Errorf("UART console label lacks %q", want)
		}
	}

	gadget := renderConsolePanel(t, NewConsoleModel("/dev/ttyGS0", true, uartSerial()))
	if !strings.Contains(gadget, "/dev/ttyGS0") {
		t.Error("gadget console label does not name /dev/ttyGS0")
	}
	if strings.Contains(gadget, "115200") {
		t.Error("UART framing shown for the gadget port, which it does not apply to")
	}
	if !strings.Contains(gadget, "USB") {
		t.Error("gadget console label does not say it is the USB port")
	}
}

// Settings is a dialog on the same page, so a change of console port must
// reach the label without a reload: it re-fetches on the console-changed event
// the serial and USB console handlers raise.
func TestConsoleDeviceLabelRefreshesOnConsoleChange(t *testing.T) {
	html := renderConsolePanel(t, NewConsoleModel("/dev/ttyS1", false, uartSerial()))

	tag := openingTag(t, html, "console-device")
	for _, want := range []string{`hx-get="/ui/console/device"`, "console-changed"} {
		if !strings.Contains(tag, want) {
			t.Errorf("device label lacks %s\ngot: %s", want, tag)
		}
	}
}

func TestConsoleModelFraming(t *testing.T) {
	for _, tc := range []struct {
		baud, data int
		parity     string
		stop       int
		want       string
	}{
		{115200, 8, "none", 1, "115200 8N1"},
		{9600, 7, "even", 2, "9600 7E2"},
		{19200, 8, "odd", 1, "19200 8O1"},
		{57600, 8, "None", 1, "57600 8N1"},
	} {
		s := config.Serial{BaudRate: tc.baud, DataBits: tc.data, Parity: tc.parity, StopBits: tc.stop}
		if got := NewConsoleModel("/dev/ttyS1", false, s).Framing; got != tc.want {
			t.Errorf("framing for %+v = %q, want %q", s, got, tc.want)
		}
	}
	if got := NewConsoleModel("/dev/ttyGS0", true, uartSerial()).Framing; got != "" {
		t.Errorf("gadget console has framing %q, want none", got)
	}
}
