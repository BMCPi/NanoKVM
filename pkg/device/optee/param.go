package optee

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ParamType selects what a parameter carries and which direction it moves.
type ParamType uint32

const (
	// ParamNone is an unused slot. Every request sends a fixed-size
	// parameter block, so the tail is padded with these.
	ParamNone ParamType = paramTypeNone

	ParamValueInput  ParamType = paramTypeValueInput
	ParamValueOutput ParamType = paramTypeValueOutput
	ParamValueInOut  ParamType = paramTypeValueInOut
)

// Param is one value parameter.
//
// A and B are 32-bit because that is what the GlobalPlatform value parameter
// is, even though the kernel's struct carries them in 64-bit fields. Widening
// them here would offer callers range a TA cannot receive.
type Param struct {
	Type ParamType
	A, B uint32
}

// Params is the fixed-size parameter block every request carries.
type Params [paramCount]Param

// UUID is a TA identifier in the octet order the TEE expects: the RFC 4122
// big-endian form, which is also the order the fields appear in a canonical
// UUID string.
type UUID [16]byte

// ParseUUID reads the canonical 8-4-4-4-12 form.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	stripped := strings.ReplaceAll(s, "-", "")
	if len(stripped) != 32 {
		return u, fmt.Errorf("optee: %q is not a UUID", s)
	}
	raw, err := hex.DecodeString(stripped)
	if err != nil {
		return u, fmt.Errorf("optee: %q is not a UUID: %w", s, err)
	}
	copy(u[:], raw)
	return u, nil
}

// MustParseUUID is ParseUUID for compile-time constants; TA UUIDs are
// literals in the source, so a bad one is a bug rather than bad input.
func MustParseUUID(s string) UUID {
	u, err := ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// String renders the canonical form.
func (u UUID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
