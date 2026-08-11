package cvi

import (
	"fmt"
	"os"
	"sync"
	"unsafe"
)

// Encoder is one VENC channel.
//
// The channel number is not an ioctl argument: cvi_vc_drv_venc_ioctl() reads
// it from iminor(), so a channel *is* an open handle on /dev/cvi_vc_encN and
// every command on that handle addresses that channel implicitly.
//
// That is also why the lock here is per-Encoder rather than the single global
// mutex JetKVM serialises all of its native calls behind: distinct channels
// are distinct file descriptors reaching distinct driver state, so there is
// nothing for them to contend over. The mutex only orders commands against
// each other on one channel, which the driver already needs (it takes a
// per-minor semaphore) and which keeps GET_STREAM from racing RELEASE_STREAM.
type Encoder struct {
	mu  sync.Mutex
	f   *os.File
	chn int
}

// OpenEncoder opens VENC channel chn. It returns an error wrapping
// ErrNotPresent when the soph-media modules are not loaded.
func OpenEncoder(chn int) (*Encoder, error) {
	f, err := openDev(fmt.Sprintf("%s%d", EncoderDev, chn))
	if err != nil {
		return nil, err
	}
	return &Encoder{f: f, chn: chn}, nil
}

// Channel reports which VENC channel this Encoder drives.
func (e *Encoder) Channel() int { return e.chn }

// Close releases the channel handle. It does not destroy the channel: call
// DestroyChn first if the pipeline is being torn down rather than restarted.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

// cmd runs an ioctl carrying a pointer to arg under the channel lock.
func (e *Encoder) cmd(req uintptr, arg unsafe.Pointer, what string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.f == nil {
		return fmt.Errorf("cvi: venc%d: %s on closed channel", e.chn, what)
	}
	if err := ioctl(e.f, req, arg); err != nil {
		return fmt.Errorf("cvi: venc%d: %s: %w", e.chn, what, err)
	}
	return nil
}

// cmdNoArg runs an argument-less ioctl under the channel lock.
func (e *Encoder) cmdNoArg(req uintptr, what string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.f == nil {
		return fmt.Errorf("cvi: venc%d: %s on closed channel", e.chn, what)
	}
	if err := ioctlNoArg(e.f, req); err != nil {
		return fmt.Errorf("cvi: venc%d: %s: %w", e.chn, what, err)
	}
	return nil
}

// CreateChn creates the channel with the given attributes.
func (e *Encoder) CreateChn(attr *VENCChnAttr) error {
	return e.cmd(vencCreateChn, unsafe.Pointer(attr), "create channel")
}

// DestroyChn destroys the channel, releasing its encoder resources.
func (e *Encoder) DestroyChn() error {
	return e.cmdNoArg(vencDestroyChn, "destroy channel")
}

// ResetChn resets the channel without destroying it, dropping any queued
// state. The next frame out will be a keyframe.
func (e *Encoder) ResetChn() error {
	return e.cmdNoArg(vencResetChn, "reset channel")
}

// SetChnAttr replaces the channel attributes on a live channel.
func (e *Encoder) SetChnAttr(attr *VENCChnAttr) error {
	return e.cmd(vencSetChnAttr, unsafe.Pointer(attr), "set channel attr")
}

// GetChnAttr reads back the current channel attributes.
func (e *Encoder) GetChnAttr() (*VENCChnAttr, error) {
	var attr VENCChnAttr
	if err := e.cmd(vencGetChnAttr, unsafe.Pointer(&attr), "get channel attr"); err != nil {
		return nil, err
	}
	return &attr, nil
}

// StartRecvFrame tells the channel to start accepting frames from whatever is
// bound upstream. n is the number of frames to encode; use RecvUnlimited to
// run until StopRecvFrame.
func (e *Encoder) StartRecvFrame(n int32) error {
	param := VENCRecvPicParam{S32RecvPicNum: n}
	return e.cmd(vencStartRecvFrame, unsafe.Pointer(&param), "start recv frame")
}

// RecvUnlimited is the StartRecvFrame count meaning "no limit", which is the
// only sensible setting for a live console.
const RecvUnlimited int32 = -1

// StopRecvFrame stops accepting frames. Already-queued frames still drain out
// through GetStream.
func (e *Encoder) StopRecvFrame() error {
	return e.cmdNoArg(vencStopRecvFrame, "stop recv frame")
}

// QueryStatus reports the channel's queue depths and encoder state.
func (e *Encoder) QueryStatus() (*VENCChnStatus, error) {
	var st VENCChnStatus
	if err := e.cmd(vencQueryStatus, unsafe.Pointer(&st), "query status"); err != nil {
		return nil, err
	}
	return &st, nil
}

// RequestIDR asks for the next encoded frame to be an IDR. Needed whenever a
// viewer joins mid-stream: with the long GOP a mostly-static console wants,
// they would otherwise wait seconds for a decodable frame.
func (e *Encoder) RequestIDR() error {
	return e.cmdNoArg(vencRequestIDR, "request IDR")
}

// SetRcParam replaces the rate-controller parameters, which is how quality is
// traded against bandwidth without restarting the channel.
func (e *Encoder) SetRcParam(p unsafe.Pointer) error {
	return e.cmd(vencSetRcParam, p, "set rc param")
}

// SetJpegParam replaces the JPEG quantisation parameters. Only meaningful on
// a channel created with the JPEG payload type.
func (e *Encoder) SetJpegParam(p unsafe.Pointer) error {
	return e.cmd(vencSetJpegParam, p, "set jpeg param")
}

// GetStream retrieves one encoded frame.
//
// The ioctl argument is a VENCStreamEx pointing at a caller-owned VENCStream,
// and the driver copies u32PackCount packs into that VENCStream's own PstPack
// array -- so packs must be preallocated by the caller and are capped at
// len(packs). timeoutMs is passed through to the driver; a negative value
// blocks indefinitely.
//
// The returned VENCStream aliases the caller's storage. Its packs describe
// where the bitstream lives in physical (ION) memory; reading the payload
// needs the mapping from BaseDev, not this call.
//
// Callers must pair every successful GetStream with ReleaseStream, or the
// encoder runs out of buffers.
func (e *Encoder) GetStream(stream *VENCStream, packs []VENCPack, timeoutMs int32) error {
	if len(packs) == 0 {
		return fmt.Errorf("cvi: venc%d: get stream: no pack storage provided", e.chn)
	}

	stream.PstPack = &packs[0]
	ex := VENCStreamEx{PstStream: stream, S32MilliSec: timeoutMs}

	// ex holds a Go pointer to stream, and stream holds one to packs. The
	// kernel dereferences both, so neither may move or be collected while
	// the ioctl runs; cmd's runtime.KeepAlive covers ex, and ex keeps the
	// rest reachable.
	return e.cmd(vencGetStream, unsafe.Pointer(&ex), "get stream")
}

// ReleaseStream returns a frame's buffers to the encoder. The stream and packs
// must be the ones a preceding GetStream filled in.
func (e *Encoder) ReleaseStream(stream *VENCStream, timeoutMs int32) error {
	ex := VENCStreamEx{PstStream: stream, S32MilliSec: timeoutMs}
	return e.cmd(vencReleaseStream, unsafe.Pointer(&ex), "release stream")
}
