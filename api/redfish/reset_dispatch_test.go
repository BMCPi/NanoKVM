package redfish

import (
	"testing"

	"github.com/stmcginnis/gofish/schemas"
)

// ForceOff must reach power.Controller.ForceOff (a ≥5 s hold), not PowerOff
// (a 300 ms press the OS is asked to honour). The distinction is the whole
// point of the reset type: a client sends ForceOff precisely when the host is
// too wedged to act on a graceful request.
//
// Nothing covered dispatch before this: the existing tests assert only that the
// advertised AllowableValues match supportedResetTypes, so ForceOff and
// GracefulShutdown sharing one branch was invisible.
func TestResetOpForDistinguishesForceOffFromGracefulShutdown(t *testing.T) {
	for _, tc := range []struct {
		reset schemas.ResetType
		want  resetOp
	}{
		{schemas.OnResetType, resetOpOn},
		{schemas.GracefulShutdownResetType, resetOpGracefulOff},
		{schemas.ForceOffResetType, resetOpForceOff},
		{schemas.ForceRestartResetType, resetOpRestart},
		{schemas.PowerCycleResetType, resetOpCycle},
		{schemas.NmiResetType, resetOpUnsupported},
	} {
		if got := resetOpFor(tc.reset); got != tc.want {
			t.Errorf("resetOpFor(%s) = %d, want %d", tc.reset, got, tc.want)
		}
	}
}

// TestResetOpForRoutesForceRestartThroughRestart pins the board-agnostic
// design's split: ForceRestart now dispatches to power.Controller.Restart
// (reset-line pulse where wired, per the operator's power.reset policy),
// while PowerCycle keeps meaning force-off+repower unconditionally. Before
// this, both ResetTypes shared resetOpCycle and were indistinguishable at
// this layer — the same shape of gap TestResetOpForDistinguishesForceOffFromGracefulShutdown
// closed for ForceOff/GracefulShutdown.
func TestResetOpForRoutesForceRestartThroughRestart(t *testing.T) {
	if got := resetOpFor(schemas.ForceRestartResetType); got != resetOpRestart {
		t.Errorf("resetOpFor(ForceRestartResetType) = %d, want resetOpRestart (%d)", got, resetOpRestart)
	}
	if got := resetOpFor(schemas.PowerCycleResetType); got != resetOpCycle {
		t.Errorf("resetOpFor(PowerCycleResetType) = %d, want resetOpCycle (%d)", got, resetOpCycle)
	}
	if resetOpRestart == resetOpCycle {
		t.Fatal("resetOpRestart and resetOpCycle must be distinct ops")
	}
}

// Every advertised reset type must dispatch somewhere. A value added to
// supportedResetTypes without a case in resetOpFor would otherwise reach the
// handler's default and 501 at runtime.
func TestEveryAdvertisedResetTypeDispatches(t *testing.T) {
	for _, rt := range supportedResetTypes {
		if resetOpFor(rt) == resetOpUnsupported {
			t.Errorf("%s is advertised in supportedResetTypes but resetOpFor has no case for it", rt)
		}
	}
}
