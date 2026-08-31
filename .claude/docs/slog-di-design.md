# Injected slog loggers — struct-based DI design

Approved 2026-08-31. Executed 2026-08-31 (Task 24: forbidigo ratchet landed,
all 24 tasks complete). Follows the logrus→slog call-site migration (commit
`4f3cf0c`): every first-party call site already logs through `log/slog` in
attr style, enforced by a depguard rule. This spec moves those call sites off
the process-global default logger onto injected `*slog.Logger` instances,
wired by hand from the composition root — no DI framework.

## Why

- **Component identity in structured output.** A `component=` attr on every
  record lets the JSON file logs be filtered per subsystem without regexing
  message prefixes.
- **Testability.** A constructor that receives its logger can be handed a
  discard handler (silent tests) or a capture handler (tests that assert on
  log output) without touching process-global state.
- **One graph, not two.** The repo already wires subsystems explicitly
  (`pkg/deps`, built once in `cmd/server/main.go`). Logging is currently the
  one dependency still reached through a global; this folds it into the
  existing graph.

## What does not change

- `pkg/logger` remains the sole owner of handler construction: destination
  (stdout console format / rotating JSON file), the shared `slog.LevelVar`,
  OTel trace correlation, the stdlib `log` redirect, and the logrus bridge
  for dependencies (go-diskfs). DI changes who *holds* loggers, never how
  they are built.
- `logger.SetLevel` keeps working for every injected logger: `Logger.With`
  shares the parent's handler, and the handler filters on the shared
  `LevelVar`. Runtime level changes from the settings UI therefore apply to
  all children at once.
- Message prefixes (`ipmi:`, `serial:`, …) are **kept** even though
  `component=` repeats the fact. The serial console at 115200 baud renders
  bare text lines; the prefix is what makes them scannable there. JSON
  readers filter on the attr and ignore the prefix.
- The logrus bridge and the stdlib redirect stay as the safety net so
  third-party log lines keep landing in the same handler.

## Root and carrier

- `cmd/server/main.go` keeps `root := logger.Init()` (already returns
  `*slog.Logger`).
- `pkg/deps.Deps` gains one field:

  ```go
  // Log is the root injected logger. Handler packages derive their
  // component logger from it at Register time; never at package init.
  Log *slog.Logger
  ```

- Main sets it once when building `*Deps`. Everything that logs receives
  either `Deps` (handler packages) or a `*slog.Logger` (subsystems, free
  functions) from main's wiring.

## Component taxonomy

One attr key, `component`, applied via `.With("component", "<name>")` at
construction. Names are short, stable, and match the existing message
prefixes where one exists:

| Component | Covers |
| --- | --- |
| `api/vm`, `api/auth`, `api/network`, `api/application`, `api/firmware`, `api/autoupdate` | the api handler packages |
| `redfish` | api/redfish |
| `ui` | ui root + ui/fragments |
| `power`, `ipmi`, `serial`, `ssh`, `hid`, `usbgadget`, `firmware`, `network`, `discovery`, `telemetry`, `timesync`, `autoupdate`, `video`, `rtc` | the pkg subsystems |
| `http` | pkg/platform/middleware request logging |
| `sysinfo` | pkg/platform/sysinfo resource sampling (added post-plan by the overview-graphs feature) |

A record's component is set exactly once (no nesting of `With("component")`
down a call chain). Where a subsystem constructs a sub-object with its own
lifecycle (rtc `Session` under the `Hub`), the child inherits the parent's
logger plus identifying attrs (`session=…`), not a new component.

## api/* and ui — handler structs

Each handler package converts its free-function gin handlers into methods on
an unexported per-package struct, built inside the existing `Register`
function:

```go
type handlers struct {
    d   *deps.Deps
    log *slog.Logger
}

func Register(api *gin.RouterGroup, d *deps.Deps) {
    h := &handlers{d: d, log: d.Log.With("component", "api/vm")}
    api.POST("/reboot", h.postReboot)
    …
}
```

- Handler bodies switch `slog.ErrorContext(c.Request.Context(), …)` →
  `h.log.ErrorContext(c.Request.Context(), …)`. Context discipline is
  unchanged: request context for request-scoped work, `d.ActionContext` for
  detached work.
- Applies to: api/vm, api/auth, api/network, api/application, api/firmware,
  api/autoupdate, api/redfish, ui (root handlers) and ui/fragments.
- `deps.Middleware` / `deps.FromContext` for templ components is untouched;
  a templ component that needs to log (none do today) would take the logger
  through its props, not from context.

## pkg subsystems — constructor field

Every subsystem with a struct-plus-constructor gains a `log *slog.Logger`
parameter, stored as a field; method bodies switch `slog.X(…)` →
`s.log.X(…)`:

power, firmware, hid, video capturer, rtc hub, serial broker, ssh server,
discovery, telemetry, usbgadget, autoupdate, ipmi, timesync, network
manager.

- Main passes `root.With("component", "<name>")` at construction.
- A constructor given a nil logger defaults to `slog.Default()` — this keeps
  hand-built test fixtures working and makes the field impossible to crash
  on. One guard line per constructor.
- Background goroutines a subsystem spawns use the struct's logger; they
  never reach for the package-level functions.

## Free-function packages

Functions that log take a `*slog.Logger` parameter (first parameter after
`ctx` where one exists). Callers thread it from their own injected logger.

- `pkg/utils`: each logging helper gains the param (`FetchURL`, cert
  generation, GOMEMLIMIT setup, …). Callers are subsystems/handlers that
  already hold a logger.
- `pkg/auth`: becomes a service struct — it already carries state
  (brute-force tracking, account file); the struct absorbs the logger and
  the api/auth + middleware callers hold the service.
- `pkg/application`, `pkg/proto`, `pkg/platform/sysinfo`, `pkg/platform/middleware`: logger
  parameter (middleware constructors return the `gin.HandlerFunc` closing
  over it).
- A function whose only logging is incidental debug output may instead drop
  the log line if it duplicates what the caller already reports — decided
  case by case in the plan, listed there explicitly.

## Exceptions (documented, permanent)

- **pkg/config** logs during `logger.Init()` — before any graph exists. It
  stays on package-level `slog` calls against the provisional handler.
- **cmd/rpiboot, cmd/v4l2probe**: standalone CLIs that never run
  `logger.Init`; stdlib `log` to stderr is correct for them.
- **pkg/logger** itself: constructs the world; uses its own handlers.

## Hard invariant: no package-init loggers

`var log = slog.Default().With(…)` at package scope is forbidden — package
init runs before `logger.Init`, so it would snapshot the wrong handler
forever. Component loggers are created only inside constructors/Register
functions, after main has the root. (The package-level `slog.Info` functions
were safe precisely because they resolve `Default()` per call; injected
loggers trade that for the component attr, so construction timing becomes
load-bearing.) The plan adds a forbidigo rule for `slog.Default` outside
pkg/logger and main once the migration lands.

## Testing

- Constructors under test receive `slog.New(slog.DiscardHandler)` for
  silence, or a `bytes.Buffer`-backed handler where the test asserts on
  output (pattern already established in api/redfish/middleware_test.go).
- pkg/logger's existing tests (console format, bridge, SetLevel, trace
  correlation) are unaffected.
- No new test infrastructure: the win is that log assertions no longer need
  `slog.SetDefault` swaps except in the documented-exception packages.

## Execution phases

Bottom-up so the tree compiles and tests pass after every phase:

1. `deps.Deps.Log` + main wiring + the nil-guard convention (small, enables
   everything else).
2. pkg subsystems, one package per task (constructor signature + field +
   call-site prefix change + caller updates in main).
3. Handler structs: api/* then ui — before the free-function phase, so
   every caller of a helper already holds a logger when the helper's
   signature changes.
4. Free-function packages (utils, application, proto, sysinfo, middleware,
   auth-as-service) — the churniest phase; each package's callers updated in
   the same task.
5. Ratchets: forbidigo rule for `slog.Default(`; spec's invariants folded
   into .claude/CLAUDE.md's logging note.

Each phase gates on: `go build ./...`, `golangci-lint run`,
`go test -race -count=1 ./...`.

## Non-goals

- No DI framework (wire/fx) — rejected in design review: build/runtime
  machinery without payoff on a 1 GHz single-core, 30 MB-rootfs target.
- No change to log destinations, formats, rotation, or the config schema.
- No message rewording beyond what the struct move mechanically requires —
  the attr decomposition from the logrus migration is final.
- Rollback: the design is additive per package; a package can revert to
  package-level `slog` calls independently without breaking others.
