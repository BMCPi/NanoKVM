// Package optee is a pure-Go client for the Linux TEE subsystem, enough of
// the GlobalPlatform TEE Client API to drive an OP-TEE trusted application
// without cgo or libteec.
//
// It talks to /dev/teeN by ioctl(2) directly, the same way cmd/rpiboot talks
// to usbfs, so a consumer cross-compiles for any Linux target the rest of
// this repo builds for. libteec would drag in cgo and a shared library that
// has to exist on the target.
//
// Scope is deliberately the subset an OP-TEE pseudo-TA session needs: open a
// context, open a session by UUID with a public login, invoke commands whose
// parameters are values, and cancel or close. Memory-reference parameters are
// not implemented — they would require the shared-memory allocation and
// registration ioctls, and nothing here passes buffers. Invoke returns
// ErrParamMemref rather than silently mis-sending such a request.
package optee

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"unsafe"
)

// ErrNoDevice is returned when no OP-TEE device node could be found.
var ErrNoDevice = errors.New("optee: no OP-TEE device found")

// ErrParamMemref reports a parameter this package cannot send.
var ErrParamMemref = errors.New("optee: memory-reference parameters are not supported")

// devicePattern matches the client device nodes and, importantly, not
// teepriv0: that one belongs to the supplicant and rejects the client ioctls.
var devicePattern = regexp.MustCompile(`^tee[0-9]+$`)

// Context is an open handle to one TEE device.
type Context struct {
	f *os.File
}

// Open finds the first OP-TEE device and opens it, mirroring what
// TEEC_InitializeContext does with a NULL name: walk the candidate nodes and
// take the first whose TEE_IOC_VERSION reports the OP-TEE implementation.
func Open() (*Context, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, fmt.Errorf("optee: scan /dev: %w", err)
	}
	names := make([]string, 0, 4)
	for _, e := range entries {
		if devicePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	// ReadDir is already sorted, but the ordering is load-bearing (the first
	// OP-TEE node wins) so it is not left to the caller's filesystem.
	sort.Strings(names)

	var firstErr error
	for _, name := range names {
		ctx, err := OpenDevice(filepath.Join("/dev", name))
		if err == nil {
			return ctx, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, ErrNoDevice
}

// OpenDevice opens one named device and verifies it is an OP-TEE node.
func OpenDevice(path string) (*Context, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("optee: open %s: %w", path, err)
	}
	var v versionData
	if err := teeIoctl(int(f.Fd()), reqVersion(), unsafe.Pointer(&v), "TEE_IOC_VERSION"); err != nil {
		f.Close()
		return nil, err
	}
	if v.ImplID != implIDOptee {
		f.Close()
		return nil, fmt.Errorf("optee: %s is TEE implementation %d, want OP-TEE (%d)",
			path, v.ImplID, implIDOptee)
	}
	return &Context{f: f}, nil
}

// Close releases the device.
func (c *Context) Close() error {
	if c == nil || c.f == nil {
		return nil
	}
	err := c.f.Close()
	c.f = nil
	return err
}

// Session is an open session to one trusted application.
type Session struct {
	ctx *Context
	id  uint32
}

// OpenSession opens a session to the TA named by uuid. login is one of the
// TEE login classes; only LoginPublic needs no client UUID, which is why it
// is the only one this package can currently satisfy.
func (c *Context) OpenSession(uuid UUID, login uint32) (*Session, error) {
	if c == nil || c.f == nil {
		return nil, errors.New("optee: context is closed")
	}
	buf := openSessionBuf{
		Arg: openSessionArg{
			UUID:      uuid,
			ClntLogin: login,
			NumParams: paramCount,
		},
	}
	if err := c.submit(reqOpenSession(), unsafe.Pointer(&buf), unsafe.Sizeof(buf),
		"TEE_IOC_OPEN_SESSION"); err != nil {
		return nil, err
	}
	if buf.Arg.Ret != Success {
		return nil, &Error{Op: "OpenSession", Code: buf.Arg.Ret, Origin: buf.Arg.RetOrigin}
	}
	return &Session{ctx: c, id: buf.Arg.Session}, nil
}

// submit wraps an arg buffer in a bufData and issues the ioctl.
//
// The kernel reads the arg buffer through the address stored in BufPtr, which
// is a Go pointer the runtime cannot see through a uint64 field. KeepAlive
// holds the buffer past the syscall; Go's collector does not move heap
// objects, so recording the address once is sound for the call's duration.
func (c *Context) submit(req uintptr, arg unsafe.Pointer, size uintptr, what string) error {
	data := bufData{
		BufPtr: uint64(uintptr(arg)),
		BufLen: uint64(size),
	}
	err := teeIoctl(int(c.f.Fd()), req, unsafe.Pointer(&data), what)
	runtime.KeepAlive(arg)
	return err
}

// Close closes the session. Closing an already-closed session is a no-op, so
// a deferred Close beside an explicit one is safe.
func (s *Session) Close() error {
	if s == nil || s.ctx == nil || s.ctx.f == nil {
		return nil
	}
	arg := closeSessionArg{Session: s.id}
	err := teeIoctl(int(s.ctx.f.Fd()), reqCloseSession(), unsafe.Pointer(&arg),
		"TEE_IOC_CLOSE_SESSION")
	s.ctx = nil
	return err
}

// Invoke calls command cmd in the trusted application.
//
// params is updated in place: parameters declared as outputs carry the TA's
// values back. cancelID, when non-zero, names the request so a concurrent
// Cancel can interrupt it — the TA has to cooperate for that to take effect.
func (s *Session) Invoke(cmd uint32, cancelID uint32, params *Params) error {
	if s == nil || s.ctx == nil || s.ctx.f == nil {
		return errors.New("optee: session is closed")
	}
	buf := invokeBuf{
		Arg: invokeArg{
			Func:      cmd,
			Session:   s.id,
			CancelID:  cancelID,
			NumParams: paramCount,
		},
	}
	for i, p := range params {
		if p.Type > paramTypeValueInOut {
			return fmt.Errorf("%w (parameter %d)", ErrParamMemref, i)
		}
		buf.Params[i] = ioctlParam{Attr: uint64(p.Type), A: uint64(p.A), B: uint64(p.B)}
	}
	if err := s.ctx.submit(reqInvoke(), unsafe.Pointer(&buf), unsafe.Sizeof(buf),
		"TEE_IOC_INVOKE"); err != nil {
		return err
	}
	for i := range params {
		if params[i].Type == ParamValueOutput || params[i].Type == ParamValueInOut {
			// The kernel carries value parameters in 64-bit fields, but
			// a GlobalPlatform value parameter is 32-bit; a TA cannot
			// have set anything in the high half.
			params[i].A = uint32(buf.Params[i].A) //nolint:gosec // GP value params are 32-bit
			params[i].B = uint32(buf.Params[i].B) //nolint:gosec // GP value params are 32-bit
		}
	}
	if buf.Arg.Ret != Success {
		return &Error{Op: fmt.Sprintf("Invoke(%d)", cmd), Code: buf.Arg.Ret, Origin: buf.Arg.RetOrigin}
	}
	return nil
}

// Cancel asks the TEE to abandon the request tagged with cancelID.
//
// Safe to call from another goroutine while Invoke is blocked: the ioctl runs
// on its own thread against the same descriptor. Whether the pending call
// actually returns is up to the TA, which has to poll for cancellation — so
// callers should treat this as a request, not a guarantee.
func (s *Session) Cancel(cancelID uint32) error {
	if s == nil || s.ctx == nil || s.ctx.f == nil {
		return errors.New("optee: session is closed")
	}
	arg := cancelArg{CancelID: cancelID, Session: s.id}
	return teeIoctl(int(s.ctx.f.Fd()), reqCancel(), unsafe.Pointer(&arg), "TEE_IOC_CANCEL")
}
