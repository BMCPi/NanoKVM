package components

// power_menu_test guards the reset control's word choice: it must always say
// what pressing it will actually do. "Reset" when it will pulse the
// dedicated GPIO line, "Power cycle" when it will force-off and repower
// instead (see resetActionLabel and resetIsLine in power_menu.templ, and the
// board-agnostic design's §1 dispatch table) — an operator reading "Reset"
// must never get a destructive force-off+repower instead.

import (
	"context"
	"strings"
	"testing"
)

func TestResetActionLabelReflectsWiring(t *testing.T) {
	if got := resetActionLabel(true); got != "Reset" {
		t.Errorf("resetActionLabel(true) = %q, want %q", got, "Reset")
	}
	if got := resetActionLabel(false); got != "Power cycle" {
		t.Errorf("resetActionLabel(false) = %q, want %q", got, "Power cycle")
	}
}

func renderPowerActionGroup(t *testing.T, on, resetLine bool) string {
	t.Helper()

	var sb strings.Builder
	if err := powerActionGroup(on, resetLine).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestPowerActionGroupRendersResetLabelForWiring pins the templ integration
// point: powerActionGroup must thread its resetLine argument all the way
// into the rendered button text, not just accept it.
func TestPowerActionGroupRendersResetLabelForWiring(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resetLine  bool
		wantLabel  string
		otherLabel string
	}{
		{"reset line wired under a policy that uses it", true, "Reset", "Power cycle"},
		{"no reset line, or policy forces cycle", false, "Power cycle", "Reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := renderPowerActionGroup(t, true, tc.resetLine)

			if !strings.Contains(html, tc.wantLabel) {
				t.Errorf("missing label %q\ngot: %s", tc.wantLabel, html)
			}
			if strings.Contains(html, tc.otherLabel) {
				t.Errorf("stale label %q leaked into the rendered group\ngot: %s", tc.otherLabel, html)
			}
		})
	}
}
