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
	"log"
	"time"

	"github.com/pi-bmc/nanokvm-app/pkg/video"
	"github.com/pi-bmc/nanokvm-app/pkg/video/v4l2"
)

func main() {
	dur := flag.Duration("t", 8*time.Second, "how long to stream")
	codec := flag.String("codec", "h264", "h264|h265|mjpeg")
	flag.Parse()

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

	cap, err := v4l2.Open("")
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer cap.Close()

	if err := cap.Start(video.Config{Codec: c}); err != nil {
		log.Fatalf("start: %v", err)
	}

	deadline := time.After(*dur)
	var frames, bytes, keyframes int
	var firstPTS, lastPTS time.Duration
	for {
		select {
		case f := <-cap.Frames():
			if frames == 0 {
				firstPTS = f.PTS
				log.Printf("first frame: %d bytes, keyframe=%v", len(f.Data), f.Keyframe)
			}
			frames++
			bytes += len(f.Data)
			lastPTS = f.PTS
			if f.Keyframe {
				keyframes++
			}
		case s := <-cap.States():
			log.Printf("state: ready=%v streaming=%v %dx%d@%.0f err=%q",
				s.Ready, s.Streaming, s.Width, s.Height, s.FramePerSecond, s.Err)
		case <-deadline:
			st := cap.State()
			log.Printf("done: %d frames (%d key) %d bytes in %s, pts span %s, dropped=%d",
				frames, keyframes, bytes, *dur, lastPTS-firstPTS, cap.DroppedFrames())
			log.Printf("final state: ready=%v %dx%d err=%q", st.Ready, st.Width, st.Height, st.Err)
			if frames == 0 && !st.Ready {
				log.Printf("no signal at the bridge — is an HDMI source connected?")
			}
			return
		}
	}
}
