package components

// power_menu_test guards the shape and the honesty of the power action grid.
//
// Shape: a two-column grid of four buttons — toggle, Reset, Force Off, Force
// reset — with Force reset last. Honesty: the reset control always reads
// "Reset" and always works, following the operator's power.reset policy
// through (*power.Controller).Restart — a reset-line pulse where wired and
// allowed, else force-off+repower. An RPi 5 host exposes only a power
// button, so on that fleet the policy is cycle and Reset must not go grey or
// relabel itself "Power cycle"; its title is what says which of the two it
// will do (see the board-agnostic design's §1 dispatch table).

import (
	"context"
	"strings"
	"testing"
)

func renderPowerActionGroup(t *testing.T, on, resetLine bool) string {
	t.Helper()

	var sb strings.Builder
	if err := powerActionGroup(on, resetLine).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// buttonPosting returns the rendered <button> element (opening tag through
// its content) whose hx-post is path. Substring surgery rather than an HTML
// parser: the fragment is small, templ's output is regular, and pulling in
// x/net/html for four buttons is not worth a direct dependency.
func buttonPosting(t *testing.T, html, path string) (tag, element string) {
	t.Helper()

	for _, chunk := range strings.Split(html, "<button")[1:] {
		end := strings.Index(chunk, ">")
		if end < 0 {
			continue
		}
		tag = "<button" + chunk[:end+1]
		if strings.Contains(tag, `hx-post="`+path+`"`) {
			return tag, "<button" + chunk
		}
	}
	t.Fatalf("no button posts to %s\ngot: %s", path, html)
	return "", ""
}

// hasBoolAttr reports whether the opening tag carries name as a bare boolean
// attribute. Every button's class list has disabled: variants and every power
// button carries hx-disabled-elt, so a plain substring match would lie.
func hasBoolAttr(tag, name string) bool {
	return strings.Contains(tag, " "+name+" ") || strings.Contains(tag, " "+name+">")
}

func TestPowerActionGroupIsATwoByTwoGrid(t *testing.T) {
	html := renderPowerActionGroup(t, true, true)

	if !strings.Contains(html, "grid-cols-2") {
		t.Errorf("power actions are not laid out as a two-column grid\ngot: %s", html)
	}
	if n := strings.Count(html, "<button"); n != 4 {
		t.Errorf("rendered %d buttons, want 4 (toggle, reset, force off, force reset)\ngot: %s", n, html)
	}
	if strings.Contains(html, "button-group-separator") {
		t.Error("welded separators leaked into the grid; a grid cell is its own outlined button")
	}
}

// Force reset is the last option: it renders after every other action, so it
// lands bottom-right in the 2x2 grid.
func TestPowerActionGroupForceResetIsLastOption(t *testing.T) {
	html := renderPowerActionGroup(t, true, true)

	last := strings.Index(html, `hx-post="/ui/power/forcereset"`)
	if last < 0 {
		t.Fatalf("no Force reset action in the grid\ngot: %s", html)
	}
	for _, path := range []string{"/ui/power/off", "/ui/power/reset", "/ui/power/forceoff"} {
		i := strings.Index(html, `hx-post="`+path+`"`)
		if i < 0 {
			t.Errorf("missing action %s", path)
			continue
		}
		if i > last {
			t.Errorf("%s renders after Force reset; Force reset must be the last option", path)
		}
	}
}

// Force reset always force-offs a possibly running host and repowers it, so
// it carries the same destructive confirm and tone as Force Off.
func TestForceResetIsDestructiveAndConfirmed(t *testing.T) {
	tag, element := buttonPosting(t, renderPowerActionGroup(t, true, true), "/ui/power/forcereset")

	if !strings.Contains(element, ">Force reset<") {
		t.Errorf("Force reset button is not labelled \"Force reset\"\ngot: %s", element)
	}
	for _, want := range []string{"hx-confirm=", "data-confirm-destructive", "text-destructive"} {
		if !strings.Contains(tag, want) {
			t.Errorf("Force reset button lacks %s\ngot: %s", want, tag)
		}
	}
}

// attrValue returns the value of the double-quoted attribute name in an
// opening tag, or "" when the tag does not carry it.
func attrValue(tag, name string) string {
	_, after, ok := strings.Cut(tag, " "+name+`="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(after, `"`)
	return v
}

// The reset control keeps its word and stays live whatever the wiring, and
// its title says what pressing it will actually do — a line pulse or a
// force-off+repower — so the operator can still tell the two apart.
func TestResetControlFollowsPolicyAndSaysWhich(t *testing.T) {
	titles := map[bool]string{}
	for _, tc := range []struct {
		name      string
		resetLine bool
		wantHint  string
	}{
		{"reset line wired under a policy that uses it", true, "stays powered"},
		{"no reset line, or policy forces cycle", false, "power.reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := renderPowerActionGroup(t, true, tc.resetLine)
			tag, element := buttonPosting(t, html, "/ui/power/reset")

			if hasBoolAttr(tag, "disabled") {
				t.Errorf("reset control is disabled; it must follow power.reset instead\ngot: %s", tag)
			}
			if !strings.Contains(element, ">Reset<") {
				t.Errorf("reset control is not labelled \"Reset\"\ngot: %s", element)
			}
			if strings.Contains(html, "Power cycle") {
				t.Error("the reset control relabelled itself \"Power cycle\"; the word stays \"Reset\" and the title carries the behaviour")
			}
			title := attrValue(tag, "title")
			if !strings.Contains(title, tc.wantHint) {
				t.Errorf("reset control title %q does not say %q", title, tc.wantHint)
			}
			titles[tc.resetLine] = title
		})
	}
	if titles[true] == titles[false] {
		t.Errorf("reset control describes a line pulse and a force-off+repower identically: %q", titles[true])
	}
}
