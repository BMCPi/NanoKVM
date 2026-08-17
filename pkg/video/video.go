// Package video defines the platform-neutral interface to the BMC's HDMI
// capture pipeline: configure the encoder, watch the input signal, and pull
// encoded frames out for WebRTC or a snapshot endpoint.
//
// The interface deliberately mirrors the surface JetKVM arrived at for the
// same job (EDID get/set, codec selection, a quality factor, a streaming
// status, and a pushed state carrying resolution/fps) -- that shape is proven
// by a shipping product, and a KVM genuinely needs all of it: the UI has to
// react when the host changes resolution or the cable is pulled, not just
// when frames stop.
//
// What is NOT borrowed is the transport. JetKVM's encoder SDK is a userspace
// library (Rockchip libmpp), so their frames cross a cgo boundary and get
// copied per frame. On the SG2002 the vendor compiles its VPU SDK into the
// kernel module instead, so the whole VI -> VPSS -> VENC path is bound
// in-kernel and an implementation here can hand out frames without cgo and
// without a copy. See pkg/video/v4l2, which drives the soph_v4l2
// kernel module through the standard V4L2 API.
package video

import (
	"errors"
	"time"
)

// ErrNotSupported reports that this build or this board has no capture
// pipeline. It is deliberately an error rather than a panic: the BMC must
// still boot, serve Redfish and run the serial console on a board where video
// is unavailable or the driver stack failed to load.
var ErrNotSupported = errors.New("video: capture not supported on this platform")

// Codec is the wire format the encoder produces.
type Codec int

const (
	// CodecH264 is the default and the only codec every browser will accept
	// over WebRTC. On this SoC it is produced by the coda980 core.
	CodecH264 Codec = iota
	// CodecH265 halves the bitrate but is not negotiable in WebRTC on
	// Firefox and is hardware-gated in Chrome, so it is opt-in.
	CodecH265
	// CodecMJPEG is the fallback: bandwidth-hungry, but decodable by
	// anything and produced by a separate hardware JPEG core, which is also
	// what backs still snapshots.
	CodecMJPEG
)

func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecMJPEG:
		return "mjpeg"
	default:
		return "unknown"
	}
}

// StreamingStatus is whether the encoder is currently producing frames. It is
// independent of Ready: a valid input signal with no subscribed client is
// Ready but not streaming.
type StreamingStatus int

const (
	StreamingInactive StreamingStatus = iota
	StreamingActive
)

func (s StreamingStatus) String() string {
	if s == StreamingActive {
		return "active"
	}
	return "inactive"
}

// State describes the input signal and the encoder at a point in time.
//
// Ready means the bridge has locked onto a valid input; Width/Height/
// FramePerSecond are only meaningful when it is true. Err carries the reason
// the pipeline stopped, if it stopped -- an unplugged cable is reported as
// Ready=false with no Err, not as a failure.
type State struct {
	Ready          bool
	Streaming      StreamingStatus
	Err            string
	Width          int
	Height         int
	FramePerSecond float64
}

// Frame is one encoded access unit, ready to hand to a WebRTC track.
//
// Data aliases the pipeline's own buffer and stays valid only until the next
// receive on the frame channel. Callers that need to retain it past that --
// anything that buffers, retries or writes asynchronously -- must copy. This
// is what makes a zero-copy path possible at all; the alternative is a fresh
// allocation for every frame, which is what JetKVM's C.GoBytes callback does.
type Frame struct {
	Data     []byte
	PTS      time.Duration
	Keyframe bool
}

// Config is the requested pipeline configuration. Zero values mean "leave at
// the implementation's default".
type Config struct {
	// Width and Height are the *output* size. The scaler downsizes from
	// whatever the host is sending, so this bounds bitrate and encoder load
	// independently of the input resolution.
	Width  int
	Height int
	// FrameRate caps output frames per second.
	FrameRate int
	// Bitrate in bits per second, for the rate controller.
	Bitrate int
	// Codec selects the encoder core.
	Codec Codec
	// GOP is the keyframe interval in frames. A BMC console is nearly
	// static, so a long GOP is a large win -- but every new viewer needs a
	// keyframe before they see anything, which RequestKeyframe covers.
	GOP int
}

// Capturer is the HDMI capture pipeline.
//
// Implementations are expected to be safe for concurrent use. Frames and
// States return the same channel on every call.
type Capturer interface {
	// Start brings the pipeline up and begins encoding. It is idempotent.
	Start(cfg Config) error

	// Stop halts encoding but leaves the pipeline configured, so Start can
	// resume without re-negotiating the input.
	Stop() error

	// Close tears down the pipeline and releases the devices.
	Close() error

	// State returns the most recent known state.
	State() State

	// States delivers state transitions. It is latest-wins: a slow consumer
	// misses intermediate states but never blocks the pipeline and always
	// converges on the current one.
	States() <-chan State

	// Frames delivers encoded frames. It is buffered and lossy by design --
	// see Frame about aliasing, and DroppedFrames about what a slow consumer
	// costs. Dropping is the correct behaviour for live video: a late frame
	// is worth less than a current one.
	Frames() <-chan Frame

	// DroppedFrames counts frames discarded because the consumer of Frames
	// was not keeping up. A steadily climbing value means the encoder is
	// outrunning the network or the WebRTC writer.
	DroppedFrames() uint64

	// RequestKeyframe forces the next frame to be an IDR. Needed whenever a
	// viewer joins mid-stream, since a long GOP can otherwise leave them
	// looking at nothing for seconds.
	RequestKeyframe() error

	// SetQualityFactor scales the rate controller between 0 (lowest) and 1
	// (highest quality), letting the UI trade bandwidth for fidelity without
	// restarting the pipeline.
	SetQualityFactor(f float64) error
	QualityFactor() float64

	// SetCodec switches the encoder core. It may restart the pipeline, so
	// expect a keyframe and a brief gap in Frames.
	SetCodec(c Codec) error
	Codec() Codec

	// EDID returns the raw EDID currently presented to the host.
	EDID() ([]byte, error)

	// SetEDID replaces the EDID presented to the host, which is how the
	// available resolutions are constrained. The host generally has to
	// re-detect the display for this to take effect.
	SetEDID(edid []byte) error
}
