package components

// power_menu_test.go guards the power toggle's htmx binding.
//
// The toggle is one button whose action flips with the host's power state:
// the SSE handler rewrites its hx-post when the state changes. htmx captures
// hx-post in a closure when it first processes the node (processVerbs reads
// the attribute once and passes the captured path to every issueAjaxRequest),
// so rewriting the attribute alone changes the label and leaves the click
// posting to the old path — the button reads "Power On" and sends power off.
//
// Re-processing the node is what makes htmx drop the stale binding, and it is
// a single line that looks removable to anyone who has not hit the bug. This
// test is that line's canary. It asserts on the script text rather than on
// behaviour because the repo has no browser-test runner; the behavioural
// check is a manual browser pass.

import (
	"context"
	"strings"
	"testing"
)

// renderPowerMenuScript returns the script with its line comments stripped.
// The comments explain this very binding, so a test that searched the raw
// text would match the prose describing the call instead of the call.
func renderPowerMenuScript(t *testing.T) string {
	t.Helper()

	var sb strings.Builder
	if err := powerMenuScript().Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}

	var code []string
	for line := range strings.SplitSeq(sb.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// TestPowerToggleReprocessesAfterRewritingAction is the regression guard: a
// script that rewrites hx-post must re-process the node in the same breath.
func TestPowerToggleReprocessesAfterRewritingAction(t *testing.T) {
	script := renderPowerMenuScript(t)

	rewrite := strings.Index(script, `setAttribute('hx-post'`)
	if rewrite < 0 {
		t.Skip("the toggle no longer rewrites hx-post; nothing to rebind")
	}

	process := strings.Index(script, "htmx.process(")
	if process < 0 {
		t.Fatal("the script rewrites hx-post but never calls htmx.process: " +
			"htmx keeps the path it captured at load, so the toggle will post " +
			"the previous action after a state change")
	}
	if process < rewrite {
		t.Errorf("htmx.process() at %d runs before the hx-post rewrite at %d; "+
			"it has to re-read the attribute after the new value is set",
			process, rewrite)
	}
}

// The rebind must be guarded: renderPower runs from an SSE handler, and a
// missing htmx global would throw and abandon the rest of the update.
func TestPowerToggleRebindGuardsMissingHtmx(t *testing.T) {
	script := renderPowerMenuScript(t)

	if !strings.Contains(script, "htmx.process(") {
		t.Skip("no rebind to guard")
	}
	if !strings.Contains(script, "window.htmx") {
		t.Error("htmx.process is called without checking window.htmx first")
	}
}
