# Migrating pkg/video from vendor ioctls to the soph_v4l2 pipeline

> **Status: implemented.** `pkg/video/v4l2` is the live Capturer,
> `pkg/video/cvi` is deleted (git history keeps it), and the module loader
> parses modules.dep. What remains is the board gate below — the kernel
> module and this package have compiled and passed tests but never run on
> hardware together.

The kernel now exposes the whole capture pipeline behind the standard V4L2 /
Media Controller API (`soph_v4l2.ko` in the soph-media tree: `/dev/media0`,
four subdevs, and `/dev/video0` delivering encoded H.264/H.265/MJPEG through
vb2). Everything `pkg/video/cvi` painstakingly does from userspace — bring-up
ordering, VB pools, the pull-model encoder feed, register pokes — now lives
behind STREAMON. This document is the migration map.

## The shape of the change

`video.Capturer` (pkg/video/video.go) stays exactly as it is; `pkg/video/rtc`
and the server are untouched. A new `pkg/video/v4l2` implements the interface
against `/dev/video0`, and `pkg/video/cvi` is deleted. Rough line count: the
cvi package is ~4,900 lines plus generated types; the v4l2 implementation
should land around 700 (a small ioctl shim, a frame pump, a supervisor).

| Capturer method | v4l2 implementation |
| --- | --- |
| `Start(cfg)` | `S_FMT` (fourcc from `cfg.Codec`, size from cfg), `S_PARM` (fps), `S_CTRL` bitrate/GOP, `SUBSCRIBE_EVENT(SOURCE_CHANGE)`, `REQBUFS(4, MMAP)` + mmap, `QBUF`×4, `STREAMON` |
| `Stop()` | `STREAMOFF` — the kernel tears the vendor pipeline down, identical to stock's idle-stop; buffers stay mapped for the next `Start` |
| `Frames()` | pump goroutine: `poll(POLLIN\|POLLPRI)` → `DQBUF` → `Frame{Data, PTS: timestamp, Keyframe: V4L2_BUF_FLAG_KEYFRAME}` → lossy buffered channel (same drop semantics and counter as today) → `QBUF` |
| `States()` | `POLLPRI` → `DQEVENT`: on `SOURCE_CHANGE`, `QUERY_DV_TIMINGS` + `ENUM_INPUT` status → publish `State`; no timer polling — the kernel's bridge poller is the detector now |
| `RequestKeyframe()` | `S_CTRL(V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME)` |
| `SetCodec` / `SetQualityFactor` | stop → `S_FMT` / bitrate ctrl → start (the kernel encoder cannot re-negotiate RC live; a rebuild-on-change matches current behavior) |
| `EDID`/`SetEDID` | unchanged: they already return `ErrNotSupported`; boot-time `EnsureEDID` stays (see below) |

## What gets deleted

All of the vendor-MPI machinery in `pkg/video/cvi`, none of which has a
remaining job:

- `pipeline.go`, `pipeline_setup.go` — bring-up/teardown ordering (now
  `soph_pipeline.c`)
- `capturer.go`'s frame loop — VPSS pull → VENC feed (now the kernel feeder
  thread; back-pressure now originates at vb2 buffer availability)
- `vb.go`, `ispmem.go`, `snrinfo.go` — VB pools / ISP mem / sensor geometry
- `pinmux.go`, `clocks.go`, `laneoverride.go` — register pokes (now in-kernel)
- `venc.go`, `bitstream.go` — encoder ioctls and the /dev/mem bitstream
  windows (`DQBUF` hands us the bytes; no `CONFIG_DEVMEM` dependency left)
- `errno.go`, `ioctl.go`, `gen_types.go`, `types_linux.go` — vendor ABI
- `console.go` — printk throttling; the kernel driver does not error-flood

`pkg/video/lt6911` shrinks to EDID provisioning only (Signal/FrameRate/
StartOutput move behind the kernel subdev).

## The pieces that need real design attention

**1. Module loading (`modules.go`).** Two changes:

- The list grows and renames: `mc videodev videobuf2-common videobuf2-v4l2
  videobuf2-memops videobuf2-vmalloc` (in-tree, dependency order), then the
  renamed vendor family `soph_sys … soph_vc_driver`, then `soph_v4l2`.
- `findModule` only searches `{extra, kernel, .}/<name>.ko` **flat** — the
  V4L2 core modules live under `kernel/drivers/media/...`, so it will not
  find them. Replace the search with a `modules.dep` parse: it gives exact
  paths *and* dependency order for free, shrinking the hardcoded list to the
  leaf modules (`soph_v4l2` and the vendor chain).

The image has no kmod/modprobe, so raw `finit_module` stays; note the
deployment coupling — the app binary and the renamed-module image must ship
together.

**2. EDID vs. kernel i2c ownership.** `soph_v4l2` holds i2c-4/0x2b via a
dummy client and its poller does bank-switched transactions; a concurrent
userspace `I2C_SLAVE_FORCE` write would interleave with those and corrupt
both. The safe sequence is ordering, not locking: run `EnsureEDID()` **before
inserting `soph_v4l2`** (i2c-4 is driver-free at that point), then never
touch the bus from userspace while the module is loaded. Runtime `SetEDID`
stays `ErrNotSupported` until it moves into the kernel subdev.

**3. Failure/rebuild semantics.** The cvi supervisor's jobs map onto V4L2
signals one-for-one:

- frame-loop death → `DQBUF` returns `EIO` (`vb2_queue_error`): STREAMOFF →
  STREAMON rebuilds, same as today's rebuild-on-frameFailed
- resolution change → `SOURCE_CHANGE` event: STREAMOFF → `QUERY_DV_TIMINGS`
  → `S_FMT` → STREAMON
- cable pull → STREAMON fails `ENOLINK` / event with no lock: publish
  `Ready=false`, wait for the next event (no polling loop)
- wedged bridge → the kernel pulses the LT6911 hardware reset itself
  (`bridge_reset=recover`); the app just sees the retry succeed or fail

**4. Frame aliasing.** Keep today's contract (Data valid until the next
receive) by copying `bytesused` into a reusable buffer exactly as
`capturer.go` does now, or go zero-copy by deferring the `QBUF` of buffer N
until frame N+1 is taken — with 4 buffers there is slack for either. Start
with the copy (identical semantics, no new invariants); measure before
optimizing.

**5. Constructor wiring.** `cmd/server/main.go` swaps the cvi constructor
for `v4l2.New` under the same build tags; `video.Unsupported` remains the
fallback, so a board without the module still boots to Redfish and serial.

## Sequencing

1. Land `pkg/video/v4l2` + `modules.dep` loader behind a flag, cvi intact.
2. Board-verify with `v4l2-ctl` first (no app in the loop), then the flag.
3. Flip the default, delete `pkg/video/cvi`, shrink `lt6911` to EDID.

Step 2 is the gate: the kernel module has compiled but never run on the
board, and every timing claim in it (feeder pacing, source-change flow,
recovery reset) wants hardware confirmation before the app bets on it.
