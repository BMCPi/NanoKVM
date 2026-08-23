package optee

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseUUID(t *testing.T) {
	u, err := ParseUUID("575d6607-5a2b-4384-827e-c16a25ac4fa5")
	if err != nil {
		t.Fatalf("ParseUUID: %v", err)
	}
	if u[0] != 0x57 || u[15] != 0xa5 {
		t.Errorf("octets = % x", u[:])
	}
	// Dashes are cosmetic; the octets are what matters.
	bare, err := ParseUUID("575d66075a2b4384827ec16a25ac4fa5")
	if err != nil || bare != u {
		t.Errorf("undashed form = %v, %v; want the same UUID", bare, err)
	}
	for _, bad := range []string{"", "not-a-uuid", "575d6607-5a2b-4384-827e-c16a25ac4f", "zz5d6607-5a2b-4384-827e-c16a25ac4fa5"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Errorf("ParseUUID(%q) succeeded, want an error", bad)
		}
	}
}

// OpenDevice must reject anything that is not an OP-TEE node rather than
// carrying on and issuing session ioctls at it. A regular file answers the
// version ioctl with ENOTTY, which is also the proof that the request number
// reaches the kernel and is understood as a request this file cannot serve.
func TestOpenDeviceRejectsNonTEEFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nottee")
	if err := os.WriteFile(path, []byte("not a device"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenDevice(path)
	if err == nil {
		t.Fatal("OpenDevice on a regular file succeeded, want an error")
	}
	if !errors.Is(err, unix.ENOTTY) && !errors.Is(err, unix.EINVAL) {
		t.Errorf("error = %v, want ENOTTY from the version ioctl", err)
	}
	if !strings.Contains(err.Error(), "TEE_IOC_VERSION") {
		t.Errorf("error = %v, should name the failing ioctl", err)
	}
}

func TestOpenDeviceReportsMissingNode(t *testing.T) {
	_, err := OpenDevice(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want a not-exist error", err)
	}
}

// A closed handle must refuse work rather than issue an ioctl on a freed
// descriptor, which would either fail obscurely or hit an unrelated file.
func TestClosedHandlesRefuseWork(t *testing.T) {
	var ctx *Context
	if _, err := ctx.OpenSession(UUID{}, LoginPublic); err == nil {
		t.Error("OpenSession on a nil context succeeded")
	}
	if err := (&Context{}).Close(); err != nil {
		t.Errorf("closing an empty context = %v, want nil", err)
	}
	var sess *Session
	if err := sess.Close(); err != nil {
		t.Errorf("closing a nil session = %v, want nil", err)
	}
	params := Params{}
	if err := sess.Invoke(0, 0, &params); err == nil {
		t.Error("Invoke on a nil session succeeded")
	}
	if err := sess.Cancel(0); err == nil {
		t.Error("Cancel on a nil session succeeded")
	}
}

// Memory references would need shared-memory allocation this package does not
// do. Sending one as if it were a value would hand the TA a pointer-shaped
// integer, so it is refused before the ioctl.
func TestInvokeRefusesMemrefParams(t *testing.T) {
	sess := &Session{ctx: &Context{f: os.Stdin}}
	params := Params{{Type: ParamValueInput}, {Type: ParamType(5)}}
	err := sess.Invoke(1, 0, &params)
	if !errors.Is(err, ErrParamMemref) {
		t.Errorf("error = %v, want ErrParamMemref", err)
	}
}

func TestErrorMessageNamesCodeAndOrigin(t *testing.T) {
	e := &Error{Op: "Invoke(1)", Code: ErrItemNotFound, Origin: OriginTEE}
	got := e.Error()
	for _, want := range []string{"Invoke(1)", "ITEM_NOT_FOUND", "0xffff0008", "TEE"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, should contain %q", got, want)
		}
	}
}

func TestIsCancelled(t *testing.T) {
	if !IsCancelled(&Error{Code: ErrCancel}) {
		t.Error("ErrCancel should read as cancelled")
	}
	if !IsCancelled(&Error{Code: ErrExternalCancel}) {
		t.Error("ErrExternalCancel should read as cancelled")
	}
	if IsCancelled(&Error{Code: ErrGeneric}) {
		t.Error("ErrGeneric should not read as cancelled")
	}
	if IsCancelled(errors.New("unrelated")) {
		t.Error("a non-TEE error should not read as cancelled")
	}
}
