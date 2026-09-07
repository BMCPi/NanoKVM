package fragments

// power_test pins which power.Controller method each /ui/power/:action
// dispatches to. h.d.Power is a concrete *power.Controller that needs real
// GPIO, so the handler itself is unreachable from a test; the dispatch table
// is a pure value instead (the same move api/redfish made with resetOpFor)
// and this compares the functions it holds.

import (
	"context"
	"reflect"
	"testing"

	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

// sameFunc reports whether a and b are the same function. Go only compares
// funcs to nil, so this goes through their code pointers; method expressions
// on the same method share one.
func sameFunc(a, b func(*power.Controller, context.Context) error) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// Reset and Force reset are the UI's two resets, and they must stay two: the
// first follows the operator's power.reset policy (Restart — the reset line
// where wired), the second is the unconditional force-off+repower (Reset),
// mirroring Redfish ForceRestart vs PowerCycle.
func TestPowerActionsDistinguishResetFromForceReset(t *testing.T) {
	reset, ok := powerActions["reset"]
	if !ok {
		t.Fatal("no reset action")
	}
	if !sameFunc(reset.run, (*power.Controller).Restart) {
		t.Error("reset does not dispatch to Restart (policy-aware reset)")
	}

	force, ok := powerActions["forcereset"]
	if !ok {
		t.Fatal("no forcereset action")
	}
	if force.label != "Force reset" {
		t.Errorf("forcereset toast label = %q, want %q", force.label, "Force reset")
	}
	if !sameFunc(force.run, (*power.Controller).Reset) {
		t.Error("forcereset does not dispatch to Reset (unconditional force-off+repower)")
	}
}

// Every button the power menu renders must have an action behind it: an
// unknown action is a 400 with a toast, which the menu must never provoke.
func TestPowerActionsCoverEveryMenuButton(t *testing.T) {
	for _, action := range []string{"on", "off", "reset", "forceoff", "forcereset"} {
		a, ok := powerActions[action]
		if !ok {
			t.Errorf("menu action %q has no handler entry", action)
			continue
		}
		if a.label == "" || a.run == nil {
			t.Errorf("action %q is incomplete: label=%q run=%v", action, a.label, a.run != nil)
		}
	}
}
