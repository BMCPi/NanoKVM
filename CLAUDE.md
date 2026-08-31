# NanoKVM App

Go BMC/KVM server for NanoKVM hardware (riscv64 target). Gin HTTP + templ server-rendered UI, Redfish/IPMI, WebRTC video.

## Commands

- `make app` - full build: go generate → clean → `golangci-lint fmt` → riscv64 build (+upx)
- `go generate ./...` - regenerates `*_templ.go` from `ui/**/*.templ` (via `go tool templ generate`); never edit `*_templ.go` by hand
- `golangci-lint fmt ./...` - format; `golangci-lint run` - lint (config: .golangci.yml, v2)
- `go test -race -count=1 ./...` - tests (CI uses gotestsum with CGO_ENABLED=1)
- `make deploy KVM_HOST=<ip>` - upload built server to a device via offline update (defaults in Makefile)

## CI gotchas

- The `generate` CI job reruns `go generate ./...` + `golangci-lint fmt` and fails on any diff or untracked file — always commit regenerated templ output alongside `.templ` changes.

## Layout

- `cmd/` entrypoints (server, rpiboot) · `api/` HTTP handlers · `pkg/` domain packages · `ui/` templ pages/components/fragments
- UI components vendored via shadcn-templ (`components.json`); add new ones with `shadcn-templ add <name>`

## Device constraints

- On-device `/tmp` is a ~30MB RAM overlay — never route uploads/ISOs through `os.TempDir()`; use `pkg/streamio` streaming helpers (`StreamMultipartFile`, `FetchURL`)

## Logging

- log/slog only (depguard blocks logrus outside pkg/logger). Loggers are
  injected: handlers use their struct's `h.log`, subsystems their `s.log`,
  helpers a `log` parameter. Never `slog.Default()`/package-level `slog.X`
  in first-party code (forbidigo), and never build a logger at package init.
- pkg/config, pkg/logger, cmd/rpiboot, cmd/v4l2probe are the documented
  exceptions. Details: .claude/docs/slog-di-design.md.

## Subsystem designs

@discovery-design.md
