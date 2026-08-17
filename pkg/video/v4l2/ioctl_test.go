package v4l2

import (
	"testing"
	"unsafe"
)

// The struct sizes pin the ABI: every value here was verified against the
// kernel's own uapi headers (a C program printing sizeof for each). A drift
// in any layout changes the computed ioctl request number and would show up
// as ENOTTY on the board — this test catches it at build time instead.
func TestABILayouts(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"capability", unsafe.Sizeof(capability{}), 104},
		{"format", unsafe.Sizeof(format{}), 208},
		{"streamParm", unsafe.Sizeof(streamParm{}), 204},
		{"control", unsafe.Sizeof(control{}), 8},
		{"requestBuffers", unsafe.Sizeof(requestBuffers{}), 20},
		{"buffer", unsafe.Sizeof(buffer{}), 88},
		{"eventSubscription", unsafe.Sizeof(eventSubscription{}), 32},
		{"event", unsafe.Sizeof(event{}), 136},
		{"input", unsafe.Sizeof(input{}), 80},
		{"dvTimings", unsafe.Sizeof(dvTimings{}), 132},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}

	// Field offsets the code reads through raw memory.
	var b buffer
	if off := unsafe.Offsetof(b.Timestamp); off != 24 {
		t.Errorf("buffer.Timestamp at %d, want 24", off)
	}
	if off := unsafe.Offsetof(b.M); off != 64 {
		t.Errorf("buffer.M at %d, want 64", off)
	}
	if off := unsafe.Offsetof(b.Length); off != 72 {
		t.Errorf("buffer.Length at %d, want 72", off)
	}
}

// Request numbers, against the values the kernel's videodev2.h macros
// produce (printed by the same C program).
func TestRequestNumbers(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"QUERYCAP", reqQuerycap(), 0x80685600},
		{"G_FMT", reqGFmt(), 0xc0d05604},
		{"S_FMT", reqSFmt(), 0xc0d05605},
		{"REQBUFS", reqReqbufs(), 0xc0145608},
		{"QUERYBUF", reqQuerybuf(), 0xc0585609},
		{"QBUF", reqQbuf(), 0xc058560f},
		{"DQBUF", reqDqbuf(), 0xc0585611},
		{"STREAMON", reqStreamon(), 0x40045612},
		{"STREAMOFF", reqStreamoff(), 0x40045613},
		{"S_PARM", reqSParm(), 0xc0cc5616},
		{"S_CTRL", reqSCtrl(), 0xc008561c},
		{"ENUMINPUT", reqEnumInput(), 0xc050561a},
		{"SUBSCRIBE_EVENT", reqSubscribeEvent(), 0x4020565a},
		{"DQEVENT", reqDqevent(), 0x80885659},
		{"QUERY_DV_TIMINGS", reqQueryDvTimings(), 0x80845663},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}
