# Board-Agnostic Host Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `.claude/docs/board-agnostic-design.md` — reset-line support with a `power.reset` policy knob, host-authoritative BIOS vocabulary, conditional RPi surfaces, a host-telemetry seam, and a packaging axis — so an Intel NUC (coreboot + custom EDK2 with the tianocore Redfish client) is fully manageable.

**Architecture:** The policy decision (line vs cycle) lives in `pkg/device/power.Controller`, which alone knows wiring and config; the three dispatch tables (Redfish, IPMI, UI) call one new `Restart` entry point. Platform vocabulary flows host→BMC through the already-implemented registry PUT; compiled-in vocabulary is deleted, not abstracted. Telemetry consumers move behind `pkg/device/hostsensor.Source`, with bmcsensor as the RPi implementation.

**Tech Stack:** Go 1.26, gpiocdev (existing), gin + templ, existing slog-DI conventions.

**Spec:** `.claude/docs/board-agnostic-design.md` — the binding authority. Research evidence: `.claude/docs/rpi-specificity-map.md`, `.claude/docs/edk2-redfish-client-gaps.md`.

## Global Constraints

- Logging follows the DI conventions in `.claude/CLAUDE.md` (`h.log`/`s.log`/injected params; no package-level slog; component set once). Do not reword existing log messages except where a feature changes behavior.
- Another session commits to this tree: `git status` before starting; stage ONLY files you changed by explicit path; never `-A`/`.`/`-a`; never bare `--amend`.
- Gate per task: `go build ./... && go vet ./... && golangci-lint run && go test -count=1 <covering pkgs>` (scoped `-race` allowed and encouraged for pkg/device/power and hostsensor concurrency; never the full-repo race suite).
- templ: edit `.templ` sources only, run `go generate ./...`, commit regenerated `*_templ.go` alongside (CI fails on any diff).
- Config compatibility: existing `/etc/kvm/server.yaml` files must load unchanged; new knobs default to today's behavior (`power.reset: auto` behaves identically on the RPi wiring; `hardware.fanControl` defaults true on RPi profiles).
- Commit per task with the message given; end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: Reset line + policy in pkg/device/power and pkg/config

**Files:**
- Modify: `pkg/device/power/power.go` (Controller gains reset support; read the button-press mechanics first and mirror their gpiocdev request/hold/release discipline)
- Modify: `pkg/config/types.go` (~259-270: `Power` gains `Reset string \`yaml:"reset"\``), config validation/defaults (accept `""`→`auto`, reject other values with a clear error), `pkg/config/hardware.go` context (GPIOReset already resolved at :74 — no change expected, verify)
- Test: `pkg/device/power/power_test.go`, config tests

**Interfaces (produced, later tasks consume):**
- `(*Controller).ResetLine(ctx context.Context) error` — momentary ~250ms active pulse of the profile's `GPIOReset`; returns a wrapped "reset line not wired" sentinel error (exported: `power.ErrNoResetLine`) when the profile resolves no pin.
- `(*Controller).CanResetLine() bool`
- `(*Controller).Restart(ctx context.Context) error` — policy: mode `line` → ResetLine (error if unwired, NEVER substitute a cycle); `cycle` → existing `Reset(ctx)`; `auto` → ResetLine if wired else `Reset(ctx)`.
- Constructor threading: `NewController` already receives `config.Power` — read the new field from it.

- [ ] Step 1: Write failing tests: table across {auto,line,cycle} × {wired,unwired} asserting which underlying op runs (fake/recorded pin layer — follow the package's existing test seams for GPIO) and that `line`+unwired returns `ErrNoResetLine`; config test: absent `reset` key → `auto`; invalid value rejected at load.
- [ ] Step 2: Run: `go test -count=1 -race ./pkg/device/power/... ./pkg/config/...` — expect FAIL.
- [ ] Step 3: Implement (config field + validation; ResetLine/CanResetLine/Restart; hold duration as a named const with a comment on the 250ms choice).
- [ ] Step 4: Tests pass; full gate.
- [ ] Step 5: Commit `feat(power): reset-line support with power.reset policy (auto|line|cycle)`

### Task 2: Route the three dispatch tables through Restart

**Files:**
- Modify: `api/redfish/inventory.go` (resetOp enum ~:42-70: add `resetOpRestart`; `resetOpFor`: `ForceRestart`→`resetOpRestart`, `PowerCycle` stays `resetOpCycle`), `api/redfish/systems.go` (execute `resetOpRestart` via `h.d.Power.Restart`), existing `api/redfish/reset_dispatch_test.go`
- Modify: `pkg/protocol/ipmi/chassis.go` (:62-70: `PowerCycle` keeps force-off+repower; `ColdReset` (hard reset) → `Restart`)
- Modify: `ui/fragments/power.go` (~:78 reset action → `Restart`) and the power-menu templ source: the reset control's label reads "Reset" when `CanResetLine()` && mode != cycle, else "Power cycle" — thread the boolean through the fragment model; `go generate`.
- Test: extend `reset_dispatch_test.go` for the new op; a UI model test if the fragments package has one for power.

**Interfaces:** Consumes Task 1's `Restart`/`CanResetLine`/`ErrNoResetLine`. Redfish `ResetType@Redfish.AllowableValues` continues to list both ForceRestart and PowerCycle (verify where AllowableValues renders — inventory.go :36-40).

- [ ] Step 1: Failing test: `resetOpFor(ForceRestartResetType) == resetOpRestart`; dispatch test that resetOpRestart invokes Restart (follow the existing test's fake-controller seam).
- [ ] Step 2: Red → implement across the three tables → green.
- [ ] Step 3: `ErrNoResetLine` surfacing: Redfish returns an actionable 400 extended-info message ("reset line not wired; use PowerCycle or set power.reset"), IPMI returns a command-specific error code consistent with the HAL's existing error mapping, UI shows a toast.
- [ ] Step 4: Full gate incl. `go generate` diff-clean check. Commit `feat(reset): ForceRestart drives the reset line where wired, across Redfish/IPMI/UI`

### Task 3: Host-authoritative BIOS vocabulary

**Files:**
- Delete: `api/redfish/bios_catalog.go` and its roster test (map cites the pinning test alongside it — find by grep `bios_catalog`/roster).
- Modify: whatever served the compiled catalog as registry fallback (grep its symbols' consumers; the served registry becomes exactly the host-PUT content; pre-sync GET renders a valid empty AttributeRegistry — correct @odata.type v1_3_6, zero attributes).
- Modify: `api/redfish/ethernet_interfaces.go` (~:152): the EthConfigDxe attribute-name bridge activates only when the live registry contains those names; otherwise NIC PATCHes needing it get 400 + extended-info naming the missing attributes.
- Test: registry precedence tests (host PUT wins; empty-before-sync; deleted catalog leaves no fallback path — assert a formerly-catalog-only attribute is absent pre-sync), bridge gating tests both ways.

- [ ] Step 1: Failing tests first (the empty-registry rendering and gating).
- [ ] Step 2: Delete + rewire + green. Watch for UI attribute-rendering consumers of catalog types (grep) — they must read the host registry (typed) with untyped degradation, per spec §2.
- [ ] Step 3: Full gate. Commit `refactor(bios): host-authoritative attribute registry; drop the compiled-in catalog`

### Task 4: Conditional RPi surfaces

**Files:**
- Modify: `pkg/config/types.go` + hardware profiles (`hardware.go`): `FanControl bool \`yaml:"fanControl"\`` — profile-defaulted true for the NanoKVM/RPi profiles, false otherwise; YAML override respected.
- Modify: `api/redfish/chassis.go` (~:157): `Oem.PiBmc.FanOverrideLevel` present only when FanControl; PATCH of it when absent → 400 extended-info.
- Modify: `api/redfish/odata.go` (~:156) + `update_service.go`: drop the pinned `BiosFirmware` member-id rule; GET serves any member the host has PATCHed into SoftwareInventory; unknown → 404 as normal.
- Modify: `api/redfish/processors.go` (~:46): neutral placeholder (no ARM/A64 claim) until host sync.
- Modify: `pkg/protocol/redfish/openapi.yaml` (lines ~5,8,210) + UI copy with "Raspberry Pi" wording (map lists them) → "the managed host".
- Test: fan-knob presence/absence rendering; inventory member-id acceptance.

- [ ] Red → green per surface; full gate. Commit `feat(board): gate RPi-only surfaces on capability; neutral placeholders`

### Task 5: pkg/device/hostsensor seam

**Files:**
- Create: `pkg/device/hostsensor/hostsensor.go` — `type Reading` (re-export or alias the fields consumers actually use — derive from today's `bmcsensor.Reading` usage sites), `type Thresholds struct{ TempCeilingC, TempWarnC float64 }` (extend only if consumers need more), `type Source interface { Latest() (Reading, bool); Thresholds() Thresholds }`, and a package-level registry: `Register(s Source)` / `Get() (Source, bool)` set once from main.
- Modify: `pkg/device/bmcsensor` — implements `Source` (RPi thresholds: 100/80 move here from the UI).
- Modify consumers: `pkg/platform/telemetry/host_sensors.go`, `pkg/protocol/ipmi/hal.go` (~:29 SDR), `ui/fragments/overview.go` + overview host-sensors templ/model (thresholds from Source) — each renders/report honest absence when `Get()` reports none.
- Modify: `cmd/server/main.go` — register the bmcsensor Source only when the RPi hardware profile is active (follow how the profile is queried today); NUC path registers nothing.
- Test: consumer behavior with and without a registered Source; bmcsensor Source conformance.

- [ ] Red → green; scoped -race on hostsensor+bmcsensor+telemetry. `go generate` if templ touched. Commit `refactor(telemetry): host sensors behind pkg/device/hostsensor.Source`

### Task 6: Packaging axis + host-firmware contract doc

**Files:**
- Modify: `Makefile` (~:102) + `.goreleaser.yaml` (~:25): a `BOARD_TOOLS` variable — default keeps today's artifacts (rpiboot, bmc-sensord); `BOARD_TOOLS=` (empty) excludes both. Document in the Makefile header comment.
- Create: `.claude/docs/host-firmware-contract.md` — per spec §5: what a host firmware must implement to be updatable (capsule volume scan policy, self-flashing FMP, delete-on-apply, per-boot SoftwareInventory PATCH), plus the RHI conventions (MAC/IP pairing, unauthenticated-isolated design, custom credential lib note) drawn from `edk2-redfish-client-gaps.md`.
- Modify: `.claude/CLAUDE.md` only if a one-line pointer to the new contract doc fits the existing doc-pointer style.

- [ ] Verify `make app` still builds with default BOARD_TOOLS; gate; commit `feat(build): board tools packaging axis; document the host firmware contract`
