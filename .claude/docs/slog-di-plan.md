# Injected slog Loggers (struct DI) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move every first-party slog call site off the process-global default logger onto injected `*slog.Logger` instances carrying a `component=` attr, wired by hand from `cmd/server/main.go`.

**Architecture:** `logger.Init()` stays the sole builder of handlers/level/bridge; its returned root logger is threaded through the existing composition root (`pkg/deps.Deps` for gin handlers, constructor/entry-point parameters for pkg subsystems, function parameters for free helpers). Handler packages convert free-function gin handlers to methods on a per-package struct built in `Register`.

**Tech Stack:** Go 1.26 stdlib `log/slog` (incl. `slog.DiscardHandler`), gin, golangci-lint v2.

**Spec:** `docs/superpowers/specs/2026-08-31-slog-di-design.md` — read it first; it defines the component taxonomy, the exceptions, and the package-init invariant this plan implements.

## Global Constraints

- Component attr key is exactly `component`; values from the spec's taxonomy table (`api/vm`, `redfish`, `ui`, `power`, `ipmi`, `serial`, `ssh`, `hid`, `usbgadget`, `firmware`, `network`, `discovery`, `telemetry`, `timesync`, `autoupdate`, `video`, `rtc`, `http`). Set exactly once per logger chain — never nest a second `With("component", …)`.
- NEVER create a logger at package init (`var log = slog.Default().With(…)` is forbidden — it snapshots the pre-Init handler). Loggers are created only inside constructors/Register/Start functions.
- Message text, levels, and attrs are NOT reworded — only the receiver changes (`slog.X(…)` → `s.log.X(…)` / `h.log.X(…)`). Keep existing `ipmi:`-style message prefixes.
- Exceptions that stay on package-level slog / stdlib log: `pkg/config`, `pkg/logger`, `cmd/rpiboot`, `cmd/v4l2probe`.
- Every task gates on: `go build ./...`, `golangci-lint run`, `go test -race -count=1 <changed packages>`; the final task runs the full race suite.
- Do not edit `*_templ.go` (generated) or `ui/components/` (vendored).
- Commit after each task with the message given in the task; end commit messages with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

## Phase 1 — Root plumbing

### Task 1: `logger.Or` nil-guard + `Deps.Log` + main wiring

**Files:**

- Modify: `pkg/logger/logger.go` (add `Or`)
- Test: `pkg/logger/logger_test.go`
- Modify: `pkg/deps/deps.go` (add `Log` field)
- Modify: `cmd/server/main.go` (capture root, set `Deps.Log`)

**Interfaces:**

- Produces: `logger.Or(l *slog.Logger) *slog.Logger` — returns `l`, or `slog.Default()` when `l == nil`. Every later task's constructor uses it as its one-line guard.
- Produces: `deps.Deps.Log *slog.Logger` — the root injected logger; handler tasks derive `d.Log.With("component", …)` from it.

- [ ] **Step 1: Write the failing test** (append to `pkg/logger/logger_test.go`)

```go
func TestOr(t *testing.T) {
 own := slog.New(slog.DiscardHandler)
 if got := Or(own); got != own {
  t.Errorf("Or(non-nil) = %p, want the logger itself", got)
 }
 if got := Or(nil); got != slog.Default() {
  t.Errorf("Or(nil) = %p, want slog.Default()", got)
 }
}
```

- [ ] **Step 2: Run** `go test -run TestOr -count=1 ./pkg/logger/` — expect FAIL (`undefined: Or`).
- [ ] **Step 3: Implement** in `pkg/logger/logger.go`, below `SetLevel`:

```go
// Or returns l, or the process default logger when l is nil. It is the
// standard nil-guard for injected loggers: constructors accept a logger and
// call Or once, so hand-built test fixtures that leave the field nil keep
// working instead of panicking.
func Or(l *slog.Logger) *slog.Logger {
 if l == nil {
  return slog.Default()
 }
 return l
}
```

- [ ] **Step 4: Run** the test again — expect PASS.
- [ ] **Step 5: Add the Deps field** in `pkg/deps/deps.go`, after `Ctx`:

```go
 // Log is the root injected logger, set once by main from logger.Init's
 // return. Handler packages derive their component logger from it inside
 // Register (never at package init — see the slog-DI spec's invariant).
 Log *slog.Logger
```

- [ ] **Step 6: Wire main.** In `cmd/server/main.go`, `logger.Init()` is currently called for its side effects; capture the root and set the field where the `*deps.Deps` literal is built (search `deps.Deps{`):

```go
 root := logger.Init()
 …
 d := &deps.Deps{
  Ctx: ctx,
  Log: root,
  …
 }
```

(If main currently discards the return — `logger.Init()` bare — change that one line; keep everything else.)

- [ ] **Step 7: Gate.** `go build ./... && golangci-lint run && go test -race -count=1 ./pkg/logger/ ./pkg/deps/...` — all green.
- [ ] **Step 8: Commit** `feat(logger,deps): thread the root slog logger through the composition root`

---

## Phase 2 — pkg subsystems (constructor/entry-point field)

Every Phase-2 task follows the same cycle, with the task's own names:

1. Change the signature shown in **Interfaces**.
2. Store the logger: `log: logger.Or(l)` in the constructor / `st.log = logger.Or(l)` in the Start entry (package-state struct).
3. Enumerate the sites: `grep -rn "slog\." <package dir> --include="*.go" | grep -v _test` — convert every `slog.X(` / `slog.XContext(` to `s.log.X(` / `s.log.XContext(` (receiver name per package). Free functions inside the package that log get the struct's logger passed as a parameter or become methods — smallest edit wins.
4. Update the caller(s) named in the task (main, or another subsystem).
5. Update the package's tests to construct with `slog.New(slog.DiscardHandler)` where they now must pass a logger (nil also works via `Or` — prefer the explicit discard handler in new code).
6. Gate: `go build ./... && golangci-lint run && go test -race -count=1 <package> ./cmd/...`.
7. Commit with the task's message.

### Task 2: pkg/power

**Files:** Modify `pkg/power/power.go` (~20 sites); caller `cmd/server/main.go:161`.
**Interfaces:** `power.NewController(hw config.Hardware, pw config.Power, log *slog.Logger) *Controller` — struct gains `log *slog.Logger`; main passes `root.With("component", "power")`.

- [ ] Steps 1-7 of the Phase-2 cycle; receiver `c.log`. Methods with a `ctx` keep their `XContext` form.
- [ ] Commit `refactor(power): inject the component logger`

### Task 3: pkg/firmware

**Files:** Modify `pkg/firmware/{firmware,capsule,fetch,gadget,virtual_media}.go` (~30 sites); caller `cmd/server/main.go:162`.
**Interfaces:** `firmware.NewController(cfg *config.Config, log *slog.Logger) *Controller`; main passes `root.With("component", "firmware")`. Package-internal free functions that log (in capsule/fetch/virtual_media) take a `*slog.Logger` first-after-ctx parameter fed from the controller's field.

- [ ] Phase-2 cycle. Commit `refactor(firmware): inject the component logger`

### Task 4: pkg/hid

**Files:** Modify `pkg/hid/{hid,macro}.go` (~13 sites); caller `cmd/server/main.go:300`.
**Interfaces:** `hid.NewController(log *slog.Logger) *Controller`; main passes `root.With("component", "hid")`. The applier-lane goroutines use the controller's field.

- [ ] Phase-2 cycle. Commit `refactor(hid): inject the component logger`

### Task 5: pkg/video/rtc

**Files:** Modify `pkg/video/rtc/{hub,session}.go` (~10 sites); caller `cmd/server/main.go` `newVideoHub`.
**Interfaces:** `rtc.Options` gains `Log *slog.Logger`; `NewHub` stores `log: logger.Or(opts.Log)`. `Session` receives the hub's logger extended with its identity (`h.log.With("session", id)`) — NOT a new component (spec: children inherit).

- [ ] Phase-2 cycle; main sets `Log: root.With("component", "rtc")` in the `rtc.Options` literal at `main.go:428`.
- [ ] Commit `refactor(rtc): inject the component logger`

### Task 6: pkg/video/v4l2

**Files:** Modify `pkg/video/v4l2/capturer.go` (5 sites, currently package-level slog from the logrus migration); caller `cmd/server/main.go` `newVideoHub` (`v4l2.Open()`).
**Interfaces:** `v4l2.Open(log *slog.Logger) (*Capturer, error)` — struct gains `log`; main passes `root.With("component", "video")`.

- [ ] Phase-2 cycle. Commit `refactor(v4l2): inject the component logger`

### Task 7: pkg/ipmi

**Files:** Modify `pkg/ipmi/{ipmi,chassis}.go` (~5 sites); caller `cmd/server/main.go:171`.
**Interfaces:** `ipmi.Start(ctx context.Context, cfg *config.Config, powerCtrl *power.Controller, fwCtrl *firmware.Controller, log *slog.Logger) (*Server, error)`; main passes `root.With("component", "ipmi")`; `Server` stores it; chassis handlers use the server's field.

- [ ] Phase-2 cycle. Commit `refactor(ipmi): inject the component logger`

### Task 8: pkg/serial

**Files:** Modify `pkg/serial/{capture,broker}.go` (~12 sites); callers `cmd/server/main.go:206` (`StartCapture`) and `:512` (`StopCapture` — unchanged signature).
**Interfaces:** `serial.StartCapture(log *slog.Logger)` — the package-state capture/broker objects store `logger.Or(log)` (component `serial`); the Broker's per-session goroutines use it.

- [ ] Phase-2 cycle. Commit `refactor(serial): inject the component logger`

### Task 9: pkg/ssh

**Files:** Modify `pkg/ssh/{server,hostkey,keys,sftp}.go` (~21 sites); caller `cmd/server/main.go:216` area (`ssh.Start()`).
**Interfaces:** `ssh.Start(log *slog.Logger) error` (component `ssh`); server struct stores it; hostkey/keys/sftp free functions take the logger as a parameter from the server.

- [ ] Phase-2 cycle. Commit `refactor(ssh): inject the component logger`

### Task 10: pkg/network

**Files:** Modify `pkg/network/{manager,dhcp,isolation,netlink,rhidhcp}.go` (~46 sites); caller `cmd/server/main.go:193` (`network.Start`).
**Interfaces:** `network.Start(log *slog.Logger)` (component `network`); the manager's package-state struct stores it; the dhcpRunner and RHI goroutines receive it from the manager. `Stop`/`WaitReady` signatures unchanged.

- [ ] Phase-2 cycle. Commit `refactor(network): inject the component logger`

### Task 11: pkg/discovery

**Files:** Modify `pkg/discovery/discovery.go` (~7 sites); caller `cmd/server/main.go:229`.
**Interfaces:** `discovery.Start(log *slog.Logger) (*Responder, error)` (component `discovery`); `Restart` (called from ui/fragments/settings.go) keeps its signature — the Responder holds the logger. Do NOT touch pkg/discovery/mdns or /ssdp internals beyond what compiles — they had no first-party log sites.

- [ ] Phase-2 cycle. Commit `refactor(discovery): inject the component logger`

### Task 12: pkg/telemetry

**Files:** Modify `pkg/telemetry/{telemetry,history,metrics,routes,snapshot}.go` (~9 sites); callers `cmd/server/main.go:150,155,287,311`.
**Interfaces:** `telemetry.Init(ctx context.Context, log *slog.Logger) error` (component `telemetry`) — stores in package state; `StartSampler(ctx)`, `Middleware(r)`, `Routes(r)`, `Shutdown(ctx)` keep signatures and use the stored logger (they are already package-state functions; `Init` is their mandatory predecessor).

- [ ] Phase-2 cycle. Commit `refactor(telemetry): inject the component logger`

### Task 13: pkg/timesync

**Files:** Modify `pkg/timesync/{timesync,ntp,http}.go` (~11 sites); caller `cmd/server/main.go:221`.
**Interfaces:** `timesync.Start(log *slog.Logger)` (component `timesync`); package state stores it; ntp/http helpers take it as a parameter.

- [ ] Phase-2 cycle. Commit `refactor(timesync): inject the component logger`

### Task 14: pkg/autoupdate

**Files:** Modify `pkg/autoupdate/autoupdate.go` (~6 sites); caller `cmd/server/main.go:209`.
**Interfaces:** `autoupdate.Start(ctx context.Context, log *slog.Logger)` (component `autoupdate`); `Stop()` unchanged.

- [ ] Phase-2 cycle. Commit `refactor(autoupdate): inject the component logger`

### Task 15: pkg/usbgadget

**Files:** Modify `pkg/usbgadget/{usbgadget,build,massstorage,udc}.go` (~11 sites); caller `cmd/server/main.go:183`.
**Interfaces:** `(*Gadget).Init(log *slog.Logger) error` — the `Get()` singleton stores `logger.Or(log)` (component `usbgadget`); other methods (EthernetOn/Off, mass-storage attach) use the stored field. Callers other than main call methods, not Init, so only main changes.

- [ ] Phase-2 cycle. Commit `refactor(usbgadget): inject the component logger`

---

## Phase 3 — handler structs (api/*, redfish, ui)

Every Phase-3 task follows this cycle:

1. In the package's `Register`, build the struct FIRST:

```go
type handlers struct {
 d   *deps.Deps
 log *slog.Logger
}

func Register(api *gin.RouterGroup, d *deps.Deps) {
 h := &handlers{d: d, log: logger.Or(d.Log).With("component", "<name>")}
 // existing route lines, each handler now h.<name>
}
```

1. Convert each handler `func name(c *gin.Context)` → `func (h *handlers) name(c *gin.Context)`; inside, `slog.XContext(` → `h.log.XContext(`; plain `slog.X(` → `h.log.X(`. Free helper functions in the package that log get `log *slog.Logger` as a parameter from the method that calls them.
2. Where a handler previously took `d *deps.Deps` as an explicit parameter or fetched it via `deps.FromContext`, it now uses `h.d` (leave `deps.FromContext` alone inside templ components).
3. Gate + commit as in Phase 2.

### Task 16: api/vm

**Files:** Modify `api/vm/{vm,gpio,hid,info,shell,ssh,system,terminal,tls,video,virtual-device}.go` (~40 sites).
**Interfaces:** component `api/vm`. `Register(api *gin.RouterGroup, d *deps.Deps)` signature unchanged.

- [ ] Phase-3 cycle. Commit `refactor(api/vm): handler struct with injected logger`

### Task 17: api/redfish

**Files:** Modify `api/redfish/*.go` with log sites (`hoststate,middleware,sensors,serial_interfaces,sessions,systems,update_service,virtual_media`.go, ~18 sites) plus `redfish.go` (Register).
**Interfaces:** component `redfish`. NOTE: `hoststate.go`'s `LoadHostState`/`FlushHostState` are called from main outside Register — they gain a `log *slog.Logger` parameter (main passes `root.With("component", "redfish")`), while handler methods use the struct field. `middleware_test.go`'s capture-handler test keeps passing because the handler struct's logger defaults to the test's `slog.SetDefault` via `logger.Or(d.Log)` when the test builds Deps without Log.

- [ ] Phase-3 cycle. Commit `refactor(redfish): handler struct with injected logger`

### Task 18: api/auth, api/network, api/application, api/firmware (+ api/autoupdate note)

**Files:** Modify `api/auth/{auth,login,password}.go`, `api/network/{network,settings,wol}.go`, `api/application/{application,service,update_offline}.go`, `api/firmware/firmware.go` (~24 sites total). `api/autoupdate` has zero log sites — skip it (spec's table lists it for completeness; the struct arrives if it ever logs).
**Interfaces:** components `api/auth`, `api/network`, `api/application`, `api/firmware`. `api/auth.Register(r *gin.Engine, api *gin.RouterGroup)` gains `d *deps.Deps` (main's `api.Register` already holds it — pass it through); `api/network.Register(api *gin.RouterGroup)` likewise gains `d`.

- [ ] Phase-3 cycle per package (one commit covering the four).
- [ ] Commit `refactor(api): handler structs with injected loggers`

### Task 19: ui + ui/fragments

**Files:** Modify `ui/ui.go` (2 sites) and `ui/fragments/{firmware,media,overview,power,power_events,settings}.go` (~19 sites); fragments' own Register/wiring file.
**Interfaces:** component `ui` for both (one taxonomy row). ui/fragments handlers convert to methods on a fragments-level `handlers` struct; `patchLogging` keeps calling `logger.SetLevel` (process-wide by design — do not convert it to the injected instance).

- [ ] Phase-3 cycle. Do NOT touch `ui/components/` or any `*_templ.go`.
- [ ] Commit `refactor(ui): handler structs with injected loggers`

---

## Phase 4 — free-function packages

### Task 20: pkg/middleware

**Files:** Modify `pkg/middleware/{logging,jwt}.go` (~7 sites); callers `cmd/server/main.go:288-289`, `api/api.go:28`, `ui/ui.go:65,75,103`.
**Interfaces:** `middleware.RequestLogger(log *slog.Logger) gin.HandlerFunc`, `middleware.Recovery(log *slog.Logger) gin.HandlerFunc` (component `http`, applied by main: `middleware.RequestLogger(root.With("component", "http"))`); `middleware.CheckToken(log *slog.Logger)` / `ResolveAuth(log *slog.Logger)` where those closures log (jwt.go's single site) — callers pass `d.Log`-derived loggers. `IsAuthed`, `ClearAuthCookie`, `RequireAuth` don't log; leave their signatures alone unless the compiler disagrees.

- [ ] **Step 1: Test first** — `pkg/middleware/logging_test.go` already exercises RequestLogger; update its construction to `RequestLogger(slog.New(slog.DiscardHandler))` and add one assertion that a request logs through an injected capture handler:

```go
func TestRequestLoggerUsesInjectedLogger(t *testing.T) {
 var buf bytes.Buffer
 l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
 r := gin.New()
 r.Use(RequestLogger(l))
 r.GET("/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
 w := httptest.NewRecorder()
 r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
 if !strings.Contains(buf.String(), "http request") {
  t.Fatalf("injected logger saw no request line; buf=%q", buf.String())
 }
}
```

- [ ] **Step 2:** Run — FAIL (signature). **Step 3:** implement signatures + `logger.Or`. **Step 4:** run — PASS.
- [ ] **Step 5:** Update the five caller lines. Gate. Commit `refactor(middleware): inject loggers into middleware constructors`

### Task 21: pkg/auth → service struct

**Files:** Modify `pkg/auth/{account,brute_force,password}.go` (~11 sites); callers in `api/auth` (now a handler struct from Task 18) and any `ui/fragments` use.
**Interfaces:** `auth.NewService(log *slog.Logger) *Service` (component stays the caller's — pass `h.log` from api/auth; auth is a library, not a component: do NOT add its own `With("component", …)`). Existing exported functions become methods on `*Service`; the package-level state (brute-force map, account cache) moves into the struct. Main builds one `*auth.Service` and hands it to `api/auth`'s Register via a new `Deps`-adjacent parameter or a `Deps.Auth *auth.Service` field — add `Auth *auth.Service` to `deps.Deps` (mirrors Power/Firmware fields).

- [ ] **Step 1:** Write a test that two Services don't share brute-force state (locks in the singleton→instance move):

```go
func TestServicesIsolateBruteForceState(t *testing.T) {
 a := NewService(slog.New(slog.DiscardHandler))
 b := NewService(slog.New(slog.DiscardHandler))
 for range 10 {
  a.RecordFailedLogin("10.0.0.1")
 }
 if !a.IsLockedOut("10.0.0.1") {
  t.Fatal("a should be locked out after 10 failures")
 }
 if b.IsLockedOut("10.0.0.1") {
  t.Fatal("b must not share a's lockout state")
 }
}
```

(Adapt the method names to the package's real brute-force API — keep the *behavioral* assertion: instance state, not package state.)

- [ ] **Steps 2-4:** red → implement → green. **Step 5:** thread `Deps.Auth` from main; update callers. Gate. Commit `refactor(auth): service struct with injected logger`

### Task 22: pkg/application, pkg/proto, pkg/sysinfo

**Files:** Modify `pkg/application/{install,update,version}.go` (~12 sites), `pkg/proto/request.go` (4 sites), `pkg/sysinfo/interfaces.go` (2 sites); callers: `api/application`, `ui/fragments/overview.go`, `pkg/autoupdate` (application); the api handler packages (proto); `ui/fragments/settings.go`, `pkg/telemetry` (sysinfo).
**Interfaces:** each logging function gains `log *slog.Logger` immediately after `ctx` (or first, where no ctx): e.g. `application.Update(ctx context.Context, log *slog.Logger, …)`, `proto.ValidateRequest(c *gin.Context, log *slog.Logger, …)`, `sysinfo.Interfaces(log *slog.Logger)` — exact names come from the files; the rule is uniform. Callers pass their struct's logger (`h.log`, subsystem field, or `logger.Or(nil)` never — every caller has one by now).

- [ ] Convert, update all callers (grep each function name repo-wide), gate, commit `refactor(application,proto,sysinfo): logger parameters`

### Task 23: pkg/utils

**Files:** Modify `pkg/utils/{cert,encrypt,hdmi,http,memory}.go` (~27 sites); callers across api/, ui/, pkg/ (grep each exported name).
**Interfaces:** logging helpers gain the `log *slog.Logger` parameter: `GenerateCert(log)`, `Download(req, target, log)` → keep param order `(log *slog.Logger)` LAST for exported functions with established call shapes (smallest caller diff), first-after-ctx for ones taking ctx. `InitGoMemLimit(log)` (main), `PersistHDMIDisabled/Enabled(log)`, `Decrypt`/`DecodeDecrypt` log sites take the param only if the site logs (drop a Debug line instead if it duplicates the caller's error report — list any dropped line in the commit body).

- [ ] Convert, update all callers, gate, commit `refactor(utils): logger parameters`

---

## Phase 5 — ratchets and closure

### Task 24: forbidigo ratchet, docs, full verification

**Files:** Modify `.golangci.yml`, `.claude/CLAUDE.md`, `docs/superpowers/specs/2026-08-31-slog-di-design.md` (mark executed).

- [ ] **Step 1:** Add forbidigo to `.golangci.yml` `linters.enable` and settings:

```yaml
    forbidigo:
      forbid:
        - pattern: 'slog\.Default\('
          msg: inject a *slog.Logger (see docs/superpowers/specs/2026-08-31-slog-di-design.md); slog.Default is for pkg/logger and the documented exceptions only
```

and an exclusion rule alongside the existing ones:

```yaml
      - linters:
          - forbidigo
        path: (pkg/logger/|pkg/config/|cmd/server/main\.go)
```

- [ ] **Step 2:** Run `golangci-lint run` — expect zero issues (all `slog.Default` uses now live in excluded paths; fix any straggler it finds by injecting properly, not by widening the exclusion).
- [ ] **Step 3:** Add to `.claude/CLAUDE.md` under a `## Logging` heading:

```markdown
## Logging

- log/slog only (depguard blocks logrus outside pkg/logger). Loggers are
  injected: handlers use their struct's `h.log`, subsystems their `s.log`,
  helpers a `log` parameter. Never `slog.Default()`/package-level `slog.X`
  in first-party code (forbidigo), and never build a logger at package init.
- pkg/config, pkg/logger, cmd/rpiboot, cmd/v4l2probe are the documented
  exceptions. Details: docs/superpowers/specs/2026-08-31-slog-di-design.md.
```

- [ ] **Step 4:** Full gate: `go build ./... && golangci-lint run && go test -race -count=1 ./...` — all green.
- [ ] **Step 5:** Grep gates: `grep -rn "slog\.\(Debug\|Info\|Warn\|Error\)" --include="*.go" api/ ui/ pkg/ | grep -v "pkg/config/\|pkg/logger/\|_test.go"` returns nothing (package-level calls gone outside exceptions).
- [ ] **Step 6:** Commit `chore: ratchet injected-logger invariants (forbidigo + docs)`
