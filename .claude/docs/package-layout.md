# Package layout restructuring proposal

Status: **Proposed — awaiting approval.** Read-only research; nothing in this
repo has been moved. All numbers below come from static analysis on 2026-08-31
(`find`/`wc` for size, `grep` on import blocks for the dependency graph) —
treat file/LOC/call-site counts as accurate to within a few percent, not
compiler-verified.

**Recommendation up front:** adopt **Option B (moderate)** — group the
hardware, protocol, and app-service packages into `pkg/device/`,
`pkg/protocol/`, `pkg/app/`, and a small `pkg/platform/`, dissolve
`pkg/utils`, but leave `pkg/config`, `pkg/logger`, `pkg/deps`, and `pkg/proto`
exactly where they are. Skip `internal/` entirely. See [Recommendation](#recommendation).

## 1. Inventory

### pkg/ (29 leaf packages, 33,855 LOC, 181 files)

| Package | Files | LOC | Purpose |
| --- | ---: | ---: | --- |
| `pkg/application` | 4 | 421 | Install/update the app's own binary+assets bundle (dirs, install, update, version) |
| `pkg/auth` | 7 | 697 | Credential store, bcrypt accounts, brute-force lockout, Basic-Auth cache |
| `pkg/autoupdate` | 1 | 164 | Background ticker that polls upstream releases and applies updates |
| `pkg/bmcsensor` | 4 | 910 | Reads the host SoC sensor record pushed into the BMC's emulated I2C EEPROM |
| `pkg/config` | 10 | 2214 | Viper-backed config file, schema, migrations (incl. the discovery migration) |
| `pkg/deps` | 1 | 122 | Composition root's dependency carrier — built once by `cmd/server/main.go` |
| `pkg/discovery` (+`mdns`,`ssdp`) | 10 | 1932 | DNS-SD + SSDP self-advertisement lifecycle (see `discovery-design.md`) |
| `pkg/firmware` | 9 | 2328 | FMP capsule staging, virtual media, USB gadget mass-storage wiring |
| `pkg/hid` | 4 | 1454 | USB HID gadget functions (keyboard, mouse/pointer) |
| `pkg/identity` | 2 | 169 | Derives the BMC's stable UUID (shared by SSDP/DNS-SD/Redfish) |
| `pkg/ipmi` | 10 | 1433 | IPMI-over-LAN adapter over the BMC's own controllers |
| `pkg/logger` | 7 | 1084 | slog default logger, file/stdout destination, OTel correlation, logrus bridge |
| `pkg/middleware` | 7 | 1183 | Gin middleware: JWT, request logging, loopback-only HTTP, TLS |
| `pkg/network` | 10 | 2246 | Host-facing interface config via netlink (replaces ip/udhcpc/ifupdown) |
| `pkg/optee` | 6 | 746 | Pure-Go client for the Linux TEE subsystem (OP-TEE) |
| `pkg/power` | 2 | 940 | GPIO power management (button-press + direct modes) |
| `pkg/proto` | 6 | 208 | Shared request/response DTOs for the JSON API (consumed by `api/*` + `ui`) |
| `pkg/redfish` | 2 | 79 | Shared Redfish artifact: embedded OpenAPI 3.1 spec + YAML normalization |
| `pkg/serial` (+`circular`) | 7 | 2218 | Shared serial terminal broker (WS, IPMI SOL, Redfish all read the same port) |
| `pkg/shell` | 3 | 462 | Local shell/PTY sessions, backing both the web terminal and `pkg/ssh` |
| `pkg/ssh` | 7 | 1788 | In-process SSH server (host keys, SFTP/SCP) |
| `pkg/sysinfo` | 5 | 1155 | BMC's own identity: OS version, device key, interface addresses |
| `pkg/telemetry` | 7 | 1144 | OpenTelemetry tracing/metrics + Prometheus scrape endpoint |
| `pkg/timesync` | 4 | 471 | SNTP client + HTTP Date fallback (replaces ntpd) |
| `pkg/usbgadget` | 12 | 1547 | Sole owner of the USB device gadget's configfs tree |
| `pkg/utils` | 19 | 2665 | Grab-bag: streaming download/upload, decompression, fs perms, crypto, memlimit |
| `pkg/video` (+`lt6911`,`rtc`,`v4l2`) | 15 | 4075 | HDMI capture pipeline interface + bridge chip, WebRTC hub, V4L2 backend |

### api/ (7 packages, 14,767 LOC, 79 files) — feature-flat, already reasonable

| Package | Files | LOC | Purpose |
| --- | ---: | ---: | --- |
| `api/application` | 3 | 208 | App install/update HTTP handlers |
| `api/auth` | 3 | 200 | Login/logout/account HTTP handlers |
| `api/autoupdate` | 1 | 51 | Auto-update settings HTTP handlers |
| `api/firmware` | 1 | 348 | Firmware/capsule HTTP handlers |
| `api/network` | 3 | 363 | Network settings HTTP handlers |
| `api/redfish` | 54 | 11945 | Full Redfish service (by far the largest single package in the repo) |
| `api/vm` | 13 | 1614 | Legacy NanoKVM JSON API (gpio, hid, shell, ssh, system, video, vm) |

### cmd/ (4 binaries, 1694 LOC, 8 files)

| Binary | Files | LOC | Purpose |
| --- | ---: | ---: | --- |
| `cmd/server` | 2 | 655 | The BMC server — composition root, wires `pkg/deps` and starts everything |
| `cmd/rpiboot` | 1 | 258 | Pushes a boot payload into the Pi 5 BootROM over USB (ships on-device) |
| `cmd/bmc-sensord` | 4 | 613 | OP-TEE sensor daemon, runs on the managed host, not the BMC |
| `cmd/v4l2probe` | 1 | 168 | Board-side smoke test for the V4L2 capture pipeline |

### ui/ (templ pages/components, 309 first-party .go files repo-wide including these)

`ui/components/*` (29 vendored shadcn-templ leaves), `ui/fragments` (15,
hand-rolled panels), `ui/pages` (12), `ui/layouts` (2), plus `ui/render`,
`ui/utils`, `ui/assets`. Already well-nested by kind (components vs fragments
vs pages vs layouts) and explicitly out of scope here — see
[Constraints](#5-constraints).

## 2. Import graph

Fan-in = number of files anywhere in the repo with an import line for that
package; this is the number of call sites that break if the package moves.

**High fan-in (foundational — expensive to move):**

| Package | Fan-in | First-party deps |
| --- | ---: | --- |
| `pkg/config` | 58 | none |
| `pkg/logger` | 34 | `pkg/config` |
| `pkg/deps` | 32 | `pkg/auth`, `pkg/config`, `pkg/firmware`, `pkg/hid`, `pkg/power`, `pkg/video/rtc` |
| `pkg/firmware` | 22 | `pkg/config`, `pkg/logger`, `pkg/telemetry`, `pkg/usbgadget`, `pkg/utils` |
| `pkg/power` | 18 | `pkg/config`, `pkg/logger`, `pkg/telemetry` |
| `pkg/utils` | 15 | none |
| `pkg/proto` | 13 | none |
| `pkg/telemetry` | 12 | `pkg/config`, `pkg/logger`, `pkg/sysinfo` |

**Mid fan-in:** `pkg/application`(9), `pkg/middleware`(8), `pkg/video`(7),
`pkg/sysinfo`(7), `pkg/serial`(7), `pkg/bmcsensor`(6), `pkg/auth`(6).

**Leaf-ish (≤5, cheap to move individually):** `pkg/usbgadget`(5),
`pkg/shell`/`pkg/redfish`/`pkg/network`/`pkg/hid`(4 each),
`pkg/video/rtc`/`pkg/ssh`/`pkg/optee`/`pkg/identity`/`pkg/discovery`/`pkg/autoupdate`(3
each), `pkg/video/v4l2`/`pkg/timesync`(2 each), and single-importer packages
`pkg/video/lt6911`, `pkg/serial/circular`, `pkg/ipmi`,
`pkg/discovery/mdns`/`ssdp`.

**Notable wrinkle:** `pkg/ipmi` imports `api/redfish` (a `pkg → api`
dependency, backwards from the usual layering). Pre-existing, orthogonal to
this proposal — flagged, not something any option below fixes.

**Already-good nesting** (confirms the parent-child pattern works and should
be preserved, not undone): `pkg/video/{lt6911,rtc,v4l2}`,
`pkg/discovery/{mdns,ssdp}`, `pkg/serial/circular`. None of the options below
touch these internal relationships.

**Cross-tree:** `api/*` packages import `pkg/deps` (composition-root access)
plus their specific subsystem packages; `api/redfish` alone touches 12
different `pkg/*` packages. `ui/*` imports `pkg/proto`, `pkg/deps` (indirectly
through `api/*` types in places), and `ui/utils`/`ui/components/*` heavily
(these are the two single busiest first-party imports in the whole repo: 58
for `pkg/config`, but `ui/utils` at 36 and `ui/components/button` at 22 are
right behind it — `ui/` is not part of this proposal's scope, but its own
import volume is worth knowing when scheduling around it).

## 3. Grounding in Go guidance

Three sources, deliberately including the critical one:

**[go.dev/doc/modules/layout](https://go.dev/doc/modules/layout)** (official). Flat is the
default; nesting is explicitly framed as something you graduate into: "Larger
packages or commands may benefit from splitting off some functionality into
supporting packages." For `internal/`: "This prevents other modules from
depending on packages we don't necessarily want to expose... It's recommended
to keep packages in `internal` as much as possible." For servers
specifically: "usually a self-contained binary" with no importable API
surface — which is exactly this repo's shape (`cmd/server` producing one
binary; nothing in `/home/appkins/src/pi-bmc/nanokvm-build` or anywhere else
imports `github.com/pi-bmc/nanokvm-app` as a Go module — verified by grep).
Notably, the doc **never mentions a `pkg/` directory convention at all.**

**[go.dev/blog/package-names](https://go.dev/blog/package-names)**. The direct citation for
dissolving `pkg/utils`: packages named `util`, `common`, `misc` "provide
clients with no sense of what the package contains," and the prescribed fix
is exactly what's proposed in §4 — "look for types and functions with common
name elements and pull them into their own package." The same post also
warns against generic catch-alls named `api`/`types`/`interfaces` for "all
the APIs in a program" — relevant because it's a reason **not** to invent a
`pkg/types` or `pkg/api` dumping ground when regrouping. It does describe
using directories to group related packages, but is explicit that grouping
directories carry "no actual relationship among the packages" beyond
filesystem convenience — i.e., a grouping directory is not itself
architecture, just a label.

**[golang-standards/project-layout](https://github.com/golang-standards/project-layout)
critique** ([issue #117](https://github.com/golang-standards/project-layout/issues/117)).
Russ Cox, Go tech lead, on the 55k-star repo whose own README already
disclaims official status: even the softer claim that it documents "common
historical and emerging project layout patterns" is "not accurate" — most Go
repositories are "much simpler" and don't use a `pkg/` directory at all. The
community consensus that formed around that thread (and is echoed across
[dev.to writeups](https://dev.to/gabrielanhaia/golang-standardsproject-layout-is-not-a-standard-russ-cox-said-so-4kjh)):
the deep-taxonomy layout is fine for a large multi-team monorepo and actively
harmful as a template for anything smaller, because it front-loads structure
the codebase hasn't earned yet and produces empty or single-file
directories nobody remembers the reason for.

**The tension, stated honestly:** this repo already is `pkg/`-shaped (a
choice made before this proposal, not something in scope to reverse), it
already has 29 packages, and it already demonstrates the one form of nesting
Go guidance actually endorses — parent-child ownership
(`video/rtc` belongs to `video`, not the other way around; `discovery/mdns`
is scoped to and only used by `discovery`). What it does **not** yet have is
purpose grouping, which Go guidance treats as optional polish for a repo this
size, not a defect. The case *for* doing it here is almost entirely
`golangci-lint` friction and human navigability at 29 flat entries, not a
correctness or API-surface argument — `internal/`'s actual enforcement
benefit (stopping external imports) is moot, since nothing external imports
this module today or is expected to. That reframes the whole proposal:
**the honest driver is readability of a 29-entry flat directory, and the
right sizing question is how much churn that readability is worth** — which
is exactly what §4's three options quantify.

## 4. `pkg/utils` dissolution (applies to every option below)

`pkg/utils` is the textbook anti-pattern from the package-names post — 19
files, no shared theme beyond "didn't have an obvious home." It dissolves
the same way regardless of which option is chosen, because none of its
19 files import each other outside of it, so its wiring is independent
from the rest of the tree.

| File(s) | Functions | External callers today | Destination |
| --- | --- | --- | --- |
| `fetch.go`, `http.go`, `multipart_stream.go` (+3 tests) | `FetchURL`, `Download`, `StreamMultipartFile` | `pkg/firmware`, `pkg/application`, `api/redfish` (×2), `api/firmware`, `ui/fragments/{firmware,media}` | **new `pkg/streamio`** — these are exactly the RAM-overlay-safe streaming primitives CLAUDE.md already calls out by name ("`StreamMultipartFile`, `FetchURL`") |
| `decompress.go`, `decompress_xz.go`, `untar.go`, `unzip.go` (+4 tests) | `DecompressingReader`, `LimitDecompressedReader`, `CompressionExtensions`, `StripCompressionSuffix`, `UnTarGz`, `Unzip` | `api/redfish/virtual_media.go`, `api/firmware`, `ui/fragments/media.go`, `ui/components/virtual_media` (templ) | **`pkg/streamio`** (same package as above — every real call site that uses fetch/upload also uses decompression in the same file; splitting them would just force two import lines everywhere they're already used together) |
| `encrypt.go` | `Decrypt`, `DecodeDecrypt` | `pkg/auth/account.go`, `api/auth/password.go` | **fold into `pkg/auth`** (sole domain: decrypting stored secrets) |
| `cert.go` | `GenerateCert` | `api/vm/tls.go`, `cmd/server/main.go` | **fold into `pkg/middleware`** (which already owns `tls.go`) |
| `chmod.go`, `move_file.go` | `ChmodRecursively`, `MoveFilesRecursively` (+ unused `MoveFile`/`MoveFileCrossFS` helpers only called internally) | `pkg/application/install.go` only | **fold into `pkg/application`** (sole consumer) |
| `memory.go` | `InitGoMemLimit` (used), `SetGoMemLimit`/`GetGoMemLimit`/`DelGoMemLimit`/`IsGoMemLimitExist` (no external callers found) | `cmd/server/main.go` | **new `pkg/memlimit`** — distinct purpose (Go runtime GOMEMLIMIT tuning for a RAM-constrained device), doesn't belong bloating `main.go` |
| `hdmi.go` | `PersistHDMIDisabled`, `PersistHDMIEnabled`, `IsHdmiDisabled` | **none found anywhere in the repo** | dead code — flag for deletion in a follow-up; if kept, lands with the video group |
| `permission.go` | `HasPermission`, `AddPermission`, `EnsurePermission` | **none found anywhere in the repo** (only self-referential) | dead code — flag for deletion; if kept, folds in with `chmod.go`/`move_file.go` above |

Churn for this dissolution alone: **19 files relocated, 15 files'
import lines rewritten** (the full list: `pkg/application/{update,install}.go`,
`pkg/firmware/{fetch.go,decompress_media_test.go}`, `api/vm/tls.go`,
`api/redfish/{update_service.go,virtual_media.go}`, `pkg/auth/account.go`,
`cmd/server/main.go`, `api/auth/password.go`, `api/firmware/firmware.go`,
`ui/components/virtual_media.templ` (source, regenerate the `_templ.go`),
`ui/components/virtual_media_test.go`, `ui/fragments/{firmware,media}.go`).
Every one of those imports collapses to a single new import line (each
caller uses functions that land in exactly one destination package), so
this is a mechanical, low-risk rename — no call sites split across two new
homes.

## 5. Three candidate depths

### Option A — Minimal: utils dissolution only

```
pkg/
  application/  auth/  autoupdate/  bmcsensor/  config/  deps/
  discovery/{mdns,ssdp}/  firmware/  hid/  identity/  ipmi/  logger/
  memlimit/          # new
  middleware/  network/  optee/  power/  proto/  redfish/
  serial/{circular}/  shell/  ssh/  sysinfo/
  streamio/          # new
  telemetry/  timesync/  usbgadget/  video/{lt6911,rtc,v4l2}/
```

Everything else stays exactly where it is today.

- **Files moved:** 19 (just the utils split from §4).
- **Import churn:** ~15 files/import lines, +1 `.templ` regenerate.
- **Risk:** essentially none. Touches no file in `pkg/sysinfo`, `pkg/telemetry`,
  or `ui/` except the one `virtual_media.templ` source (a mechanical import
  swap, not logic).
- **Verdict:** safe, does not really answer the ask. 27 of 29 original
  packages are still flat; nothing about "hardware vs protocol vs platform"
  becomes any easier to find. Worth doing regardless of what else is
  decided, but not a restructuring in its own right.

### Option B — Moderate: purpose grouping, foundational packages left alone

```
pkg/
  app/
    application/  auth/  autoupdate/  firmware/  network/  timesync/
  config/                    # unmoved — 58 existing call sites
  deps/                      # unmoved — composition-root carrier, 32 call sites
  device/
    bmcsensor/  hid/  optee/  power/  usbgadget/
    serial/{circular}/
    video/{lt6911,rtc,v4l2}/
  logger/                    # unmoved — 34 existing call sites
  platform/
    memlimit/                # new
    middleware/  sysinfo/  telemetry/
    streamio/                # new
  proto/                     # unmoved — DTOs, 13 existing call sites
  protocol/
    discovery/{mdns,ssdp}/  identity/  ipmi/  redfish/  shell/  ssh/
```

Every internal parent-child relationship from today is preserved
(`video/rtc` stays a child of `video`, not hoisted out to
`protocol/webrtc` even though it's arguably network-facing — see the
deliberate deviation note below).

- **Files relocated:** 157 of 181 `pkg/` files (87%) — the 138 files in the
  four grouping buckets (`device` 50, `protocol` 34, `app` 35, and the
  `sysinfo`/`telemetry`/`middleware` slice of `platform` 19) plus the 19
  `pkg/utils` files from §4. Only `config`(10), `deps`(1), `logger`(7), and
  `proto`(6) — 24 files — stay untouched in place.
- **Import churn:** ≈150 import-line rewrites from the four grouping
  buckets (summed fan-in: device 57 + protocol 20 + app 46 + platform-slice
  27) plus the 15 from the utils dissolution ≈ **165 import lines total**,
  spread across roughly 120–150 distinct files once overlap is accounted
  for (a file that imports two moved packages gets two edited lines but
  counts once).
- **Risk:** this is where the real tradeoff lives. `pkg/sysinfo` and
  `pkg/telemetry` move (into `platform/`), and per `git log main..HEAD`
  those are two of the most recently and repeatedly touched packages on
  this branch (`feat(sysinfo): report procs_blocked`,
  `fix(sysinfo,telemetry): don't plot readings that were never taken`,
  `refactor(telemetry): inject the component logger`, all within the last
  ~10 commits). `pkg/firmware`, `pkg/usbgadget`, and `pkg/discovery` are
  also mid-branch hot spots (29, 11, and 10 changed files respectively in
  `git diff main...HEAD --stat`) and all three move too. None of this
  touches `ui/`, which is the single hottest tree on the branch (84 changed
  files in `ui/components` alone) — because `ui/` is out of scope here, that
  collision risk doesn't apply.
- **Deliberate deviation from the literal §3-style suggestion:**
  `pkg/video/rtc` (WebRTC) stays nested under `device/video/`, not hoisted
  into `protocol/` alongside `ipmi`/`ssh`/`discovery`, even though it's
  arguably "network-facing." Its actual coupling is to `video.Frame` and the
  capture-pipeline lifecycle (`Watch`/`Configure`), not to the other
  protocol packages, and it's already a clean, working example of the
  parent-child nesting Go guidance endorses. Breaking that apart for
  taxonomic purity would cost real churn (3 call sites move to a new,
  unrelated neighborhood) for no navigability gain — this is the same
  judgment call §3 flags: don't nest for nesting's sake.
- **Why `config`/`logger`/`deps`/`proto` are excluded on purpose:** these
  four account for 137 of the ~300 total `pkg/*` import references in the
  repo (58+34+32+13). Moving them is available cheaply (they have almost no
  first-party deps of their own to drag along) but buys nothing — every
  developer already knows `pkg/config` and `pkg/logger` by name, they're the
  textbook "foundational, everyone imports this" case the go.dev guidance
  explicitly favors keeping flat, and two of them are named literally inside
  `.golangci.yml`'s `depguard`/`forbidigo` path rules (see §6) — moving them
  adds required tooling-config churn for a purely cosmetic win.

### Option C — Maximal: full taxonomy + `internal/`

Everything in Option B, plus:

```
internal/
  app/         (as Option B)
  device/      (as Option B)
  platform/
    config/  deps/  logger/  memlimit/  middleware/  proto/  streamio/  sysinfo/  telemetry/
  protocol/    (as Option B)
api/           # also moves under internal/api/, or stays — see note
```

- **Files relocated:** Option B's 157, plus `config`(10)+`deps`(1)+`logger`(7)+`proto`(6)
  = 24 more, plus every file in `pkg/` and `api/` (181+79 = 260 files) gets an
  `internal/` prefix inserted into its own path regardless of whether its
  package also regrouped.
- **Import churn:** Option B's ~165, plus 137 more from moving
  `config`/`logger`/`deps`/`proto` (§5B's excluded four), plus the
  `internal/` prefix change touches **every remaining first-party import
  line in the module** — summing all `pkg/*` fan-in gives ≈302 references
  repo-wide, and `api/*` adds more on top. Realistically this is a
  full-repo mechanical rewrite: an estimated 280–300 of the repo's 309
  first-party `.go` files change at least one import line.
- **Risk:** touches literally everything simultaneously, including
  `api/redfish` (11,945 LOC, the single largest package, also mid-branch
  hot per the diffstat) and every file that imports `pkg/config` or
  `pkg/logger` — i.e., nearly the whole tree. Any concurrent work anywhere
  in the repo collides with this, not just the sysinfo/telemetry/firmware
  areas Option B is exposed to.
- **What `internal/` actually buys here:** per §3, its entire enforcement
  value is stopping code outside this module from importing these
  packages. Nothing outside this module imports it today (verified —
  `nanokvm-build` has no `go.mod` and no Go files referencing this module
  path), and there's no plan for this repo to ever be a dependency of
  another Go module — it produces one binary. So the marginal safety
  `internal/` buys is effectively zero; the entire cost of Option C over
  Option B is paid for taxonomy purity and a compiler guarantee against a
  scenario that isn't going to happen. (A cheaper variant exists — rename
  bare `pkg/` to `internal/` without doing any taxonomy work under it —
  but combining both in one move is what makes this option "maximal," and
  that combination is what's being evaluated and rejected here.)

## Recommendation

**Option B.** Reasoning, in order of weight:

1. It's the only option whose cost is proportional to its benefit. Option A
   doesn't address the actual complaint (29 flat entries). Option C spends
   ~300 import-line edits and touches nearly the entire repo to buy an
   `internal/` guarantee against an import scenario (an external module
   depending on this one) that doesn't exist and isn't anticipated.
2. It matches what the go.dev guidance actually endorses: nesting for
   parent-child ownership (already present, preserved) plus grouping
   directories for navigability, while deliberately leaving the four most
   foundational, highest-fan-in packages flat — which is the same
   flat-favoring instinct behind Russ Cox's critique in §3, applied with
   real numbers instead of a general principle.
3. It keeps the riskiest, most actively-changing tree (`ui/`, 84+ changed
   files on this branch) untouched, and localizes the WIP-collision risk to
   two small packages (`pkg/sysinfo`: 5 files, `pkg/telemetry`: 7 files)
   rather than spreading it across the whole codebase.

**Sequencing, if approved:**

1. Land the `pkg/utils` dissolution (§4) first, alone — it's independent of
   every other decision, mechanical, and low-risk regardless of what
   directory shape is chosen afterward.
2. Land `pkg/device/*` and `pkg/protocol/*` next (84 of the 138 grouped
   files) — neither touches `pkg/sysinfo` or `pkg/telemetry`, so this can
   happen immediately without waiting on the branch's active work there.
3. Land the `pkg/app/*` and `pkg/platform/*` moves last, and specifically
   coordinate the `pkg/sysinfo`/`pkg/telemetry` portion with whoever is
   actively developing those packages on this branch — either wait for the
   current sysinfo/telemetry work to land on `main` first, or do that one
   sub-move as its own single-purpose commit reviewed alongside nothing
   else, so a rebase conflict there is a rename-only conflict, not a
   rename-plus-logic conflict.
4. Do each grouping bucket as its own commit (not one giant move), so
   `git log --follow` and `git blame` stay usable per file and review diffs
   stay scoped to one bucket at a time.

## 6. Constraints for whoever executes this

- **`.golangci.yml`** has two rules keyed on literal `pkg/` paths that must
  move with any restructure that touches `pkg/logger` or `pkg/config`:
  - `depguard.rules.no-logrus.files: ["!**/pkg/logger/**"]` (line 58)
  - `exclusions.rules[].path: (pkg/logger/|pkg/config/|cmd/server/main\.go)`
    gating the `forbidigo` `slog.Default` rule (line 195)

    Under Option B these don't move (config/logger stay put) — **no edit
    needed**. Under Option C both regexes need updating to
    `internal/platform/logger/` / `internal/platform/config/`.
- **`.golangci.yml` also excludes `ui/components/` and `ui/utils/`** from
  linting/formatting (vendored shadcn-templ sources) — unaffected by any
  option here since `ui/` is out of scope.
- **`.claude/CLAUDE.md`** references `pkg/utils` by name in the device-constraints
  section ("use `pkg/utils` streaming helpers (`StreamMultipartFile`,
  `FetchURL`)") — needs updating to `pkg/streamio` under every option, since
  the utils dissolution is common to all three.
- **`.claude/discovery-design.md`** references `pkg/discovery`,
  `pkg/discovery/mdns`, `pkg/discovery/ssdp`, and `pkg/identity` by path
  throughout. Under Option B these become `pkg/protocol/discovery/{mdns,ssdp}`
  and `pkg/protocol/identity` — the doc needs a path update pass (it's short,
  ~90 lines, mechanical). Under Option A neither path changes.
- **`go generate ./...`** and the templ toolchain are untouched by every
  option — none of these packages are inputs to templ generation except the
  one `.templ` source (`ui/components/virtual_media.templ`) that imports
  `pkg/utils` functions moving into `pkg/streamio`; that file needs its
  import updated and `go generate` rerun, with the regenerated
  `_templ.go` committed per the CI gotcha already documented in CLAUDE.md.
- **CI's `generate` job** reruns `go generate ./...` + `golangci-lint fmt`
  and fails on any diff — this means whichever option is executed must
  include a full `golangci-lint fmt ./...` pass as part of the same commit
  that moves files, not a follow-up.
- **Whatever move happens, do it as its own commit(s)**, separate from
  in-flight feature work — this branch (`edk2`) is 130 commits ahead of
  `main` with heavy recent churn in `ui/`, `api/redfish`, `pkg/video`,
  `pkg/firmware`, and `pkg/ipmi`; interleaving a rename with feature commits
  in those areas is the single biggest avoidable risk here regardless of
  which option is chosen.
