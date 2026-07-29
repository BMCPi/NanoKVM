package timesync

import (
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// rtcDevices are tried in order; on this board an RTC only exists if the
// kernel gains a driver for the SG2002's RTC block, so absence is normal.
var rtcDevices = []string{"/dev/rtc0", "/dev/rtc"}

// rtc wraps a Linux RTC character device (RTC_RD_TIME/RTC_SET_TIME ioctls).
type rtc struct {
	mu sync.Mutex
	f  *os.File
}

// openRTC returns the first present RTC device, or nil when there is none.
func openRTC() *rtc {
	for _, dev := range rtcDevices {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err == nil {
			return &rtc{f: f}
		}
	}
	return nil
}

func (r *rtc) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
}

func (r *rtc) read() (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, err := unix.IoctlGetRTCTime(int(r.f.Fd()))
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(int(rt.Year)+1900, time.Month(rt.Mon+1), int(rt.Mday),
		int(rt.Hour), int(rt.Min), int(rt.Sec), 0, time.UTC), nil
}

func (r *rtc) set(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t = t.UTC()
	rt := unix.RTCTime{
		Sec:  int32(t.Second()),      //nolint:gosec // 0..59
		Min:  int32(t.Minute()),      //nolint:gosec // 0..59
		Hour: int32(t.Hour()),        //nolint:gosec // 0..23
		Mday: int32(t.Day()),         //nolint:gosec // 1..31
		Mon:  int32(t.Month() - 1),   //nolint:gosec // 0..11
		Year: int32(t.Year() - 1900), //nolint:gosec // sane years fit int32
	}
	return unix.IoctlSetRTCTime(int(r.f.Fd()), &rt)
}
