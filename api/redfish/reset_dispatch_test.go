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
		{schemas.ForceRestartResetType, resetOpCycle},
		{schemas.PowerCycleResetType, resetOpCycle},
		{schemas.NmiResetType, resetOpUnsupported},
	} {
		if got := resetOpFor(tc.reset); got != tc.want {
			t.Errorf("resetOpFor(%s) = %d, want %d", tc.reset, got, tc.want)
		}
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
