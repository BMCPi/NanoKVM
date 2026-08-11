package video

import (
	"errors"
	"testing"
	"time"
)

func TestCodecString(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		c    Codec
		want string
	}{
		{CodecH264, "h264"},
		{CodecH265, "h265"},
		{CodecMJPEG, "mjpeg"},
		{Codec(99), "unknown"},
	} {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Codec(%d).String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestStreamingStatusString(t *testing.T) {
	t.Parallel()

	if got, want := StreamingActive.String(), "active"; got != want {
		t.Errorf("StreamingActive.String() = %q, want %q", got, want)
	}
	if got, want := StreamingInactive.String(), "inactive"; got != want {
		t.Errorf("StreamingInactive.String() = %q, want %q", got, want)
	}
}

// The zero value has to be usable, because the server constructs an
// Unsupported when probing for a capture device fails and immediately starts
// selecting on its channels.
func TestUnsupportedZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var u Unsupported

	if err := u.Start(Config{}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Start() = %v, want ErrNotSupported", err)
	}
	if err := u.RequestKeyframe(); !errors.Is(err, ErrNotSupported) {
		t.Errorf("RequestKeyframe() = %v, want ErrNotSupported", err)
	}
	if _, err := u.EDID(); !errors.Is(err, ErrNotSupported) {
		t.Errorf("EDID() = %v, want ErrNotSupported", err)
	}
	// Close must succeed: it runs on the shutdown path, where returning an
	// error would make a board without video look like a failed shutdown.
	if err := u.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	if got := u.State(); got.Ready {
		t.Errorf("State().Ready = true, want false")
	}
}

// Frames/States must return the *same* channel every call -- a fresh channel
// per call would leave earlier consumers listening to something the producer
// will never write to.
func TestUnsupportedChannelsAreStable(t *testing.T) {
	t.Parallel()

	var u Unsupported

	// Assign before comparing: written as u.Frames() != u.Frames(), revive
	// folds it to a constant false expression and the test proves nothing.
	frames1, frames2 := u.Frames(), u.Frames()
	if frames1 != frames2 {
		t.Error("Frames() returned different channels across calls")
	}

	states1, states2 := u.States(), u.States()
	if states1 != states2 {
		t.Error("States() returned different channels across calls")
	}
}

// They must also block rather than yield, so a select arm on video simply
// never fires instead of spinning the consumer at 100% CPU -- which is what a
// closed channel would do.
func TestUnsupportedChannelsBlockRatherThanClose(t *testing.T) {
	t.Parallel()

	var u Unsupported

	select {
	case f := <-u.Frames():
		t.Errorf("Frames() yielded %v, want block", f)
	case s := <-u.States():
		t.Errorf("States() yielded %v, want block", s)
	case <-time.After(20 * time.Millisecond):
	}
}
