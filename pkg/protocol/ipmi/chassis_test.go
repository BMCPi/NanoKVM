package ipmi

// chassis_test covers chassisHAL.ColdReset's synchronous half: the one
// Restart outcome that never touches hardware — policy "line" on a board
// with no reset pin — must reject before detach, as a completion code the
// go-ipmi framework's codeFromErr can extract (see ColdReset's doc comment).
// Every other combination stays fire-and-forget, exercised end-to-end by
// TestEndToEnd's "chassis control" subtest.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/device/power"
)

func newChassisHAL(t *testing.T, resetPolicy string, canResetLine bool) (*chassisHAL, *fakePower) {
	t.Helper()
	fp := newFakePower(true)
	fp.canResetLine = canResetLine
	return &chassisHAL{
		root:        t.Context(),
		power:       fp,
		resetPolicy: resetPolicy,
		log:         slog.New(slog.DiscardHandler),
	}, fp
}

// TestColdResetLinePolicyUnwiredRejectsSynchronously is the case the design
// singles out: "line" must error rather than silently substitute a power
// cycle, and IPMI must surface that as a real completion code, not a
// swallowed background-goroutine log line.
func TestColdResetLinePolicyUnwiredRejectsSynchronously(t *testing.T) {
	ch, fp := newChassisHAL(t, config.PowerResetLine, false)

	err := ch.ColdReset(context.Background())

	if !errors.Is(err, power.ErrNoResetLine) {
		t.Fatalf("ColdReset() = %v, want an error wrapping power.ErrNoResetLine", err)
	}
	var cc types.CompletionCode
	if !errors.As(err, &cc) {
		t.Fatalf("ColdReset() error %v does not carry a types.CompletionCode; codeFromErr would map it to CodeUnspecifiedError", err)
	}
	if cc != types.CodeNotSupported {
		t.Errorf("completion code = %#x, want CodeNotSupported (%#x)", byte(cc), byte(types.CodeNotSupported))
	}

	// The rejection must be synchronous and must never have detached: no
	// hardware call should follow, ever.
	select {
	case call := <-fp.calls:
		t.Fatalf("unexpected power call %q — line-policy/unwired must reject before touching hardware", call)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestColdResetDispatchesToRestartOtherwise covers the combinations that
// must still run (detached, via Restart) rather than reject: a wired board
// under "line" policy, and the "auto"/"cycle" policies regardless of wiring
// (Restart itself falls back to force-off+repower for those — see
// power.Controller.Restart's doc comment).
func TestColdResetDispatchesToRestartOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resetPolicy  string
		canResetLine bool
	}{
		{"line policy, wired", config.PowerResetLine, true},
		{"auto policy, unwired", config.PowerResetAuto, false},
		{"auto policy, wired", config.PowerResetAuto, true},
		{"cycle policy, unwired", config.PowerResetCycle, false},
		{"cycle policy, wired", config.PowerResetCycle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, fp := newChassisHAL(t, tc.resetPolicy, tc.canResetLine)

			if err := ch.ColdReset(context.Background()); err != nil {
				t.Fatalf("ColdReset() = %v, want nil (dispatch is detached)", err)
			}
			fp.waitCall(t, "restart")
		})
	}
}
