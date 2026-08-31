package optee

import (
	"errors"
	"fmt"
)

// TEE result codes (GlobalPlatform, as spelled in OP-TEE's tee_client_api.h).
// The TEE reports these in the request's ret field; they are unrelated to the
// errno an ioctl fails with, which means a call can succeed at the ioctl
// layer and still have failed inside the TA.
const (
	Success           uint32 = 0x00000000
	ErrGeneric        uint32 = 0xFFFF0000
	ErrAccessDenied   uint32 = 0xFFFF0001
	ErrCancel         uint32 = 0xFFFF0002
	ErrAccessConflict uint32 = 0xFFFF0003
	ErrExcessData     uint32 = 0xFFFF0004
	ErrBadFormat      uint32 = 0xFFFF0005
	ErrBadParameters  uint32 = 0xFFFF0006
	ErrBadState       uint32 = 0xFFFF0007
	ErrItemNotFound   uint32 = 0xFFFF0008
	ErrNotImplemented uint32 = 0xFFFF0009
	ErrNotSupported   uint32 = 0xFFFF000A
	ErrNoData         uint32 = 0xFFFF000B
	ErrOutOfMemory    uint32 = 0xFFFF000C
	ErrBusy           uint32 = 0xFFFF000D
	ErrCommunication  uint32 = 0xFFFF000E
	ErrSecurity       uint32 = 0xFFFF000F
	ErrShortBuffer    uint32 = 0xFFFF0010
	ErrExternalCancel uint32 = 0xFFFF0011
	ErrTargetDead     uint32 = 0xFFFF3024
	ErrStorageNoSpace uint32 = 0xFFFF3041
)

// Error origins: which layer produced the code.
const (
	OriginAPI        uint32 = 1
	OriginComms      uint32 = 2
	OriginTEE        uint32 = 3
	OriginTrustedApp uint32 = 4
)

var codeNames = map[uint32]string{
	ErrGeneric:        "GENERIC",
	ErrAccessDenied:   "ACCESS_DENIED",
	ErrCancel:         "CANCEL",
	ErrAccessConflict: "ACCESS_CONFLICT",
	ErrExcessData:     "EXCESS_DATA",
	ErrBadFormat:      "BAD_FORMAT",
	ErrBadParameters:  "BAD_PARAMETERS",
	ErrBadState:       "BAD_STATE",
	ErrItemNotFound:   "ITEM_NOT_FOUND",
	ErrNotImplemented: "NOT_IMPLEMENTED",
	ErrNotSupported:   "NOT_SUPPORTED",
	ErrNoData:         "NO_DATA",
	ErrOutOfMemory:    "OUT_OF_MEMORY",
	ErrBusy:           "BUSY",
	ErrCommunication:  "COMMUNICATION",
	ErrSecurity:       "SECURITY",
	ErrShortBuffer:    "SHORT_BUFFER",
	ErrExternalCancel: "EXTERNAL_CANCEL",
	ErrTargetDead:     "TARGET_DEAD",
	ErrStorageNoSpace: "STORAGE_NO_SPACE",
}

var originNames = map[uint32]string{
	OriginAPI:        "API",
	OriginComms:      "COMMS",
	OriginTEE:        "TEE",
	OriginTrustedApp: "TRUSTED_APP",
}

// Error is a failure reported by the TEE rather than by the ioctl.
type Error struct {
	Op     string
	Code   uint32
	Origin uint32
}

func (e *Error) Error() string {
	name, ok := codeNames[e.Code]
	if !ok {
		name = "UNKNOWN"
	}
	origin, ok := originNames[e.Origin]
	if !ok {
		origin = fmt.Sprintf("origin %d", e.Origin)
	}
	return fmt.Sprintf("optee: %s: %s (0x%08x) from %s", e.Op, name, e.Code, origin)
}

// IsCancelled reports whether err is a request the TEE abandoned, which is
// the ordinary way a blocking invoke ends after Cancel.
func IsCancelled(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == ErrCancel || e.Code == ErrExternalCancel
}
