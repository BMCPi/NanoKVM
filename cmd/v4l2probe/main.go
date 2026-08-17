// v4l2probe is the board-side smoke test for the capture pipeline: it drives
// the same pkg/video/v4l2 code the server uses, prints every state
// transition, and counts encoded frames for a few seconds. It exists because
// the image ships no v4l-utils — and because exercising the exact production
// path tells more than v4l2-ctl would anyway.
//
// Cross-build: CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build ./cmd/v4l2probe
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
	"github.com/pi-bmc/nanokvm-app/pkg/video/v4l2"
)

// The subdev format ioctl, probe-local: production code never touches
// subdevs, but the bench wants to see the negotiated media boundaries.
// Sizes verified against the kernel headers: v4l2_subdev_format=88,
// v4l2_mbus_framefmt=48, VIDIOC_SUBDEV_G_FMT=0xc0585604.
type mbusFramefmt struct {
	Width, Height, Code, Field, ColorSpace  uint32
	YcbcrEnc, Quantization, XferFunc, Flags uint16
	_                                       [5]uint32
}

type subdevFormat struct {
	Which  uint32 // 1 = ACTIVE
	Pad    uint32
	Format mbusFramefmt
	Stream uint32
	_      [7]uint32
}

const vidiocSubdevGFmt = 0xc0585604

func dumpSubdevs() {
	for i := 0; i < 8; i++ {
		path := fmt.Sprintf("/dev/v4l-subdev%d", i)
		fd, err := unix.Open(path, unix.O_RDWR, 0)
		if err != nil {
			continue
		}
		name := "?"
		if b, err := os.ReadFile(fmt.Sprintf("/sys/class/video4linux/v4l-subdev%d/name", i)); err == nil {
			name = string(b[:len(b)-1])
		}
		for pad := uint32(0); pad < 2; pad++ {
			f := subdevFormat{Which: 1, Pad: pad}
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
				vidiocSubdevGFmt, uintptr(unsafe.Pointer(&f)))
			if errno != 0 {
				continue
			}
			log.Printf("boundary %-14s pad%d: %dx%d code=0x%04x field=%d",
				name, pad, f.Format.Width, f.Format.Height,
				f.Format.Code, f.Format.Field)
		}
		unix.Close(fd)
	}
}

// nalTypes summarises the NAL units in an Annex-B H.264 buffer — the proof
// that what arrived is real encoded video, not zeros or garbage.
func nalTypes(b []byte) string {
	out := ""
	for i := 0; i+4 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			out += fmt.Sprintf("%d ", b[i+3]&0x1f)
			i += 3
		}
	}
	return out
}

func main() {
	dur := flag.Duration("t", 8*time.Second, "how long to stream")
	codec := flag.String("codec", "h264", "h264|h265|mjpeg")
	subdevs := flag.Bool("subdevs", false, "print media boundary formats and exit")
	dump := flag.String("dump", "", "append the raw bitstream to this file")
	flag.Parse()

	if *subdevs {
		dumpSubdevs()
		return
	}

	var c video.Codec
	switch *codec {
	case "h264":
		c = video.CodecH264
	case "h265":
		c = video.CodecH265
	case "mjpeg":
		c = video.CodecMJPEG
	default:
		log.Fatalf("unknown codec %q", *codec)
	}

	capt, err := v4l2.Open()
	if err != nil {
		log.Fatalf("open: %v", err)
	}

	var sink *os.File
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			log.Fatalf("dump: %v", err)
		}
		defer f.Close()
		sink = f
	}
	// Deliberately no Close: on a fresh boot Open inserted the whole
	// module chain, and Close would unload it on exit — including
	// soph_vc_driver, whose module_exit can block on encoder state the
	// bench is in the middle of poking at. A bench tool leaves modules
	// where it found them; the next run skips what is already loaded.

	if err := capt.Start(video.Config{Codec: c}); err != nil {
		//nolint:gocritic // bench tool: a leaked dump fd on fatal exit is fine
		log.Fatalf("start: %v", err)
	}

	deadline := time.After(*dur)
	var frames, bytes, keyframes int
	var firstPTS, lastPTS time.Duration
	for {
		select {
		case f := <-capt.Frames():
			if frames == 0 {
				firstPTS = f.PTS
				log.Printf("first frame: %d bytes, keyframe=%v, nals: %s",
					len(f.Data), f.Keyframe, nalTypes(f.Data))
			}
			if sink != nil {
				_, _ = sink.Write(f.Data)
			}
			frames++
			bytes += len(f.Data)
			lastPTS = f.PTS
			if f.Keyframe {
				keyframes++
			}
		case s := <-capt.States():
			log.Printf("state: ready=%v streaming=%v %dx%d@%.0f err=%q",
				s.Ready, s.Streaming, s.Width, s.Height, s.FramePerSecond, s.Err)
		case <-deadline:
			st := capt.State()
			log.Printf("done: %d frames (%d key) %d bytes in %s, pts span %s, dropped=%d",
				frames, keyframes, bytes, *dur, lastPTS-firstPTS, capt.DroppedFrames())
			log.Printf("final state: ready=%v %dx%d err=%q", st.Ready, st.Width, st.Height, st.Err)
			if frames == 0 && !st.Ready {
				log.Printf("no signal at the bridge — is an HDMI source connected?")
			}
			return
		}
	}
}
