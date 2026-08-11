package video

// Unsupported is a Capturer for boards and builds with no capture pipeline.
// Every operation fails with ErrNotSupported and both channels stay open and
// empty, so callers can range over them without a special case.
//
// This is the null-object half of the pattern JetKVM implements with build
// tags (cgo_linux.go / cgo_notlinux.go), with one deliberate difference:
// theirs panics via panicPlatformNotSupported(). A BMC has to keep serving
// Redfish, IPMI and the serial console on a board where video never came up,
// so failing the call is right and taking the process down is not.
//
// The zero value is ready to use.
type Unsupported struct {
	frames chan Frame
	states chan State
}

var _ Capturer = (*Unsupported)(nil)

func (u *Unsupported) Start(Config) error     { return ErrNotSupported }
func (u *Unsupported) Stop() error            { return ErrNotSupported }
func (u *Unsupported) Close() error           { return nil }
func (u *Unsupported) State() State           { return State{Err: ErrNotSupported.Error()} }
func (u *Unsupported) DroppedFrames() uint64  { return 0 }
func (u *Unsupported) RequestKeyframe() error { return ErrNotSupported }

func (u *Unsupported) SetQualityFactor(float64) error { return ErrNotSupported }
func (u *Unsupported) QualityFactor() float64         { return 0 }

func (u *Unsupported) SetCodec(Codec) error { return ErrNotSupported }
func (u *Unsupported) Codec() Codec         { return CodecH264 }

func (u *Unsupported) EDID() ([]byte, error) { return nil, ErrNotSupported }
func (u *Unsupported) SetEDID([]byte) error  { return ErrNotSupported }

// Frames and States hand back channels that are never written to and never
// closed. Never closing them is the point: a closed channel yields the zero
// value forever, which would spin any `for range`/select consumer, whereas an
// open empty channel simply blocks that arm of the select and lets the rest of
// the server run.
func (u *Unsupported) Frames() <-chan Frame {
	if u.frames == nil {
		u.frames = make(chan Frame)
	}
	return u.frames
}

func (u *Unsupported) States() <-chan State {
	if u.states == nil {
		u.states = make(chan State)
	}
	return u.states
}
