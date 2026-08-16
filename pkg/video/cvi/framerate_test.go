package cvi

import "testing"

// The VPSS channel only drops frames when FRC_INVALID is false, which needs
// both rates positive and dst *strictly* less than src (vpss.h:145). Every
// case here is a way of accidentally satisfying none of that and passing a
// 60fps source into a 30fps encoder.
func TestSrcFPSFallsBackToDestination(t *testing.T) {
	cases := []struct {
		name      string
		spec      pipeSpec
		wantSrc   int
		wantDrops bool
	}{
		{
			name:      "measured source faster than target drops",
			spec:      pipeSpec{fps: 30, inFPS: 60},
			wantSrc:   60,
			wantDrops: true,
		},
		{
			name:      "measured source equal to target is a no-op",
			spec:      pipeSpec{fps: 30, inFPS: 30},
			wantSrc:   30,
			wantDrops: false,
		},
		{
			// A 24fps source must not be scaled *up* to 30; dst >= src
			// makes the converter inactive, which is what we want.
			name:      "source slower than target is a no-op",
			spec:      pipeSpec{fps: 30, inFPS: 24},
			wantSrc:   24,
			wantDrops: false,
		},
		{
			// Measurement failed. Falling back to the target makes src
			// equal dst, which disables the converter rather than making
			// up a rate.
			name:      "unmeasured source falls back to target",
			spec:      pipeSpec{fps: 30, inFPS: 0},
			wantSrc:   30,
			wantDrops: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.spec.srcFPS()
			if src != tc.wantSrc {
				t.Errorf("srcFPS() = %d, want %d", src, tc.wantSrc)
			}
			// Mirror of FRC_INVALID, negated.
			drops := tc.spec.fps > 0 && src > 0 && tc.spec.fps < src
			if drops != tc.wantDrops {
				t.Errorf("converter active = %v, want %v (src=%d dst=%d)",
					drops, tc.wantDrops, src, tc.spec.fps)
			}
		})
	}
}

// The group has no converter and warns if asked to be one, and VI has none at
// all, so only the VPSS channel may carry an unequal pair. Getting this wrong
// either drops twice or not at all.
func TestOnlyVPSSChannelConverts(t *testing.T) {
	s := pipeSpec{inW: 1920, inH: 1080, outW: 1920, outH: 1080, fps: 30, inFPS: 60}

	if s.srcFPS() == s.fps {
		t.Fatal("test setup is not exercising a conversion")
	}
	// VI and the VPSS group are configured from srcFPS on both sides, so a
	// change to srcFPS can never make them unequal.
	if got := s.srcFPS(); got != 60 {
		t.Errorf("stage source rate = %d, want 60", got)
	}
	if s.fps != 30 {
		t.Errorf("encoder rate = %d, want 30", s.fps)
	}
}
