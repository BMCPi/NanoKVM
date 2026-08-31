package components

// video_panel_test.go guards the console's layout contract.
//
// The panel is built around one idea: #video-frame IS the picture box. Its
// aspect ratio tracks the stream, so the toolbar, the status footer and the
// overlay can all be pinned to it and land on the video. The moment the frame
// stretches to the pane instead — which is what a plain flex-1 box does — every
// one of them floats over letterbox, and the absolute-pointing maths that reads
// the video's rect stops agreeing with what the operator sees.
//
// These tests hold the pieces of that arrangement that are easy to undo by
// accident: where the chrome lives, which control appears in which state, and
// the CSS that makes the frame fit rather than fill.

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func renderVideoPanel(t *testing.T) string {
	t.Helper()
	return renderToString(t, func(w *strings.Builder) error {
		return VideoPanel(`[{"urls":"stun:stun.l.google.com:19302"}]`).Render(context.Background(), w)
	})
}

// frameInner returns the markup between the frame's opening tag and the end of
// the panel — everything pinned to the picture box.
func frameInner(t *testing.T, html string) string {
	t.Helper()
	i := strings.Index(html, `id="video-frame"`)
	if i < 0 {
		t.Fatal("no #video-frame in the panel")
	}
	return html[i:]
}

// The chrome has to be inside the frame. Outside it, on the stage, it would
// span the full pane and sit over the black bars beside a 4:3 host.
func TestVideoChromeIsPinnedToThePictureBox(t *testing.T) {
	html := renderVideoPanel(t)

	stage := strings.Index(html, `id="video-stage"`)
	frame := strings.Index(html, `id="video-frame"`)
	if stage < 0 || frame < 0 || stage > frame {
		t.Fatalf("stage/frame missing or inverted (stage %d, frame %d)", stage, frame)
	}

	inner := frameInner(t, html)
	for _, id := range []string{`id="video-toolbar"`, `id="video-overlay"`, `id="video-stream"`} {
		if !strings.Contains(inner, id) {
			t.Errorf("%s is not inside the frame, so it will not track the picture", id)
		}
	}
	if !strings.Contains(inner, "btn-video-disconnect") {
		t.Error("the disconnect action is not inside the frame")
	}
}

// Fit-contain for a container, not a replaced element. A plain aspect-ratio
// box cannot do it: with width:100% the height is derived and then merely
// clamped, which letterboxes instead of narrowing the box.
func TestVideoFrameFitsRatherThanFills(t *testing.T) {
	html := renderVideoPanel(t)

	if !strings.Contains(html, "container-type: size") {
		t.Error("the stage is not a size container, so the frame has no cqh/cqw to " +
			"measure the fit against")
	}
	if !strings.Contains(html, "100cqh") || !strings.Contains(html, "100cqw") {
		t.Error("the frame does not size itself from the stage's own box")
	}
	if !strings.Contains(html, "--video-aspect") {
		t.Error("no aspect variable: the frame cannot follow the stream's shape")
	}
	// A frame that stretches is the bug this whole arrangement exists to avoid.
	frameTag := regexp.MustCompile(`<div[^>]*id="video-frame"[^>]*>`).FindString(html)
	if strings.Contains(frameTag, "h-full") || strings.Contains(frameTag, "flex-1") {
		t.Errorf("the frame is told to fill its parent, which defeats the fit: %s", frameTag)
	}
}

// Connect belongs in the middle of a dark panel, Disconnect in the corner of a
// live one. They are never both on screen, which is why neither has to reason
// about the other.
func TestConnectAndDisconnectLiveInTheirOwnStates(t *testing.T) {
	html := renderVideoPanel(t)

	overlay := html[strings.Index(html, `id="video-overlay"`):]
	if !strings.Contains(overlay, "btn-video-connect") {
		t.Error("Connect is not in the overlay, so a disconnected panel offers no " +
			"obvious way to start")
	}

	toolbar := regexp.MustCompile(`(?s)id="video-toolbar".*?</div>\s*<div`).FindString(html)
	for _, id := range []string{"btn-video-connect", "btn-video-disconnect"} {
		if strings.Contains(toolbar, id) {
			t.Errorf("%s is back in the toolbar; the toolbar is for presentation "+
				"controls only", id)
		}
	}

	// Disconnect ships hidden — it appears only once there is a session to end.
	discTag := regexp.MustCompile(`<button[^>]*id="btn-video-disconnect"[^>]*>`).FindString(html)
	if discTag == "" {
		t.Fatal("no disconnect button")
	}
	if !strings.Contains(regexp.MustCompile(`class="[^"]*"`).ReplaceAllString(discTag, ""), "hidden") {
		t.Error("the disconnect button renders visible on a panel with no session")
	}
}

// "Discreet" cannot mean "unreachable". The two resize controls are the reason
// the toolbar exists at all.
func TestToolbarKeepsTheResizeControls(t *testing.T) {
	html := renderVideoPanel(t)
	toolbar := html[strings.Index(html, `id="video-toolbar"`):]

	for _, id := range []string{"btn-video-expand", "btn-video-fullscreen", "btn-video-mouse-mode"} {
		if !strings.Contains(toolbar, id) {
			t.Errorf("%s is missing from the toolbar", id)
		}
	}
	if !strings.Contains(html, "toggleVideoExpand") {
		t.Error("expand has no handler")
	}
}

// Tailwind v4 emits hover variants inside @media (hover:hover), which guards
// the reveal but not the hide — so a group-hover implementation leaves these
// controls permanently invisible on a touch device. The pointerless exception
// is the half that is easy to drop while "simplifying" this to a utility.
func TestToolbarStaysReachableWithoutAPointer(t *testing.T) {
	html := renderVideoPanel(t)

	if !strings.Contains(html, "hover: none") {
		t.Error("no pointerless fallback: on a touch device the toolbar would be " +
			"hidden with no hover to reveal it")
	}
	if !strings.Contains(html, "#video-frame:hover #video-toolbar") {
		t.Error("no hover reveal for the toolbar")
	}
	if !strings.Contains(html, "#video-frame:focus-within #video-toolbar") {
		t.Error("no keyboard reveal: tabbing to fullscreen would not show it")
	}
	// The Tailwind variant alone is the failure mode described above.
	if strings.Contains(html, "group-hover/video:opacity-100") {
		t.Error("the reveal is back on a bare group-hover utility, which never " +
			"applies where (hover: none)")
	}
}

// Status is a Badge and the dark state is an Empty — the shared components,
// not local lookalikes that drift the first time the palette moves.
func TestVideoPanelUsesTheSharedComponents(t *testing.T) {
	html := renderVideoPanel(t)

	if !strings.Contains(html, `data-slot="empty"`) {
		t.Error("the disconnected state is hand-rolled rather than an Empty")
	}
	badges := strings.Count(html, `data-slot="badge"`)
	if badges != 4 {
		t.Errorf("found %d badges, want one per connection state (4)", badges)
	}
	for _, state := range []string{"disconnected", "connecting", "connected", "nosignal"} {
		if !strings.Contains(html, `id="video-status-`+state+`"`) {
			t.Errorf("no status pill for %q", state)
		}
	}
}

// Exactly one state is visible at a time, and the one that ships visible is
// the one that is true before anything has connected.
func TestOnlyTheDisconnectedStatusStartsVisible(t *testing.T) {
	html := renderVideoPanel(t)

	for _, tc := range []struct {
		state      string
		wantHidden bool
	}{
		{"disconnected", false},
		{"connecting", true},
		{"connected", true},
		{"nosignal", true},
	} {
		tag := regexp.MustCompile(`<span[^>]*id="video-status-` + tc.state + `"[^>]*>`).FindString(html)
		if tag == "" {
			t.Errorf("no element for %s", tc.state)
			continue
		}
		bare := regexp.MustCompile(`class="[^"]*"`).ReplaceAllString(tag, "")
		if got := strings.Contains(bare, "hidden"); got != tc.wantHidden {
			t.Errorf("%s hidden = %v, want %v", tc.state, got, tc.wantHidden)
		}
	}
}
