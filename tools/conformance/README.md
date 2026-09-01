# Redfish fleet conformance

The standard test list every NanoKVM BMC node must pass, plus a fleet runner
that converges non-conforming nodes. Runs from a workstation, out of band of
app changes — pure shell (`bash`, `curl`, `jq`, `openssl`; `avahi-browse` for
discovery, `make`+Go toolchain for deploys).

```sh
# one node
tools/conformance/redfish-conformance.sh 10.0.137.123

# the fleet: classify, update stale failers, retest, diagnose current failers
tools/conformance/fleet-check.sh --hosts my-hosts.txt
tools/conformance/fleet-check.sh --discover            # via mDNS _redfish._tcp
tools/conformance/fleet-check.sh --converge HOST...    # also update stale passers
tools/conformance/fleet-check.sh --no-update HOST...   # classify + diagnose only
```

## The standard list (redfish-conformance.sh)

Nine sections, ~87 checks. Every mutation restores the state it found; nothing
in the default run reboots the host or leaves state behind.

1. **Transport & auth** — service root over HTTPS without auth, RedfishVersion
   pinned to 1.13.0 (bmclib floor), HTTP→HTTPS redirect, 401 for missing/bad
   credentials, Basic accepted, `$metadata` and `openapi.json` served.
2. **Collections** — Systems/Managers/Chassis each exactly `.../1`; Registries
   present; unimplemented services (EventService, AccountService, TaskService)
   answer 404.
3. **Computer system** — PowerState valid, Reset action advertises exactly the
   five supported ResetTypes, boot-override allowables complete, UEFI mode,
   link topology, all sub-resources GETtable (Bios, Settings, BootOptions,
   SecureBoot, Memory, Processors, Storage with `1`+`BMC` subsystems).
   Host-reported identity and BIOS attributes when the host has pushed them.
4. **Manager** — BMC type/branding, UUID matches service root, FirmwareVersion
   is a real build (not the `1.0.0` unstamped fallback), eth0 with MAC/IPv4/
   link-up, serial 115200-8N1 (string-typed), VirtualMedia CD with insert/eject
   actions and coherent inserted state, Dell OEM shim present.
5. **Chassis/thermal/sensors** — status OK, SoCTemp sensor freshness
   (`Oem.NanoKVM.{Stale,TemperatureValid,LastPushOK}`), plausible temperature,
   ActiveCooler fan.
6. **Update service** — capsule push URI + SimpleUpdate advertised; firmware
   inventory entry updateable, version consistent with `BiosVersion`.
7. **Session lifecycle** — POST → 201 + X-Auth-Token + Location, token works,
   DELETE answers 204. (Tokens are stateless JWTs: revocation is deliberately
   not asserted.)
8. **Write protocol negatives** — invalid ResetType → 400 (safe), host-lane
   fields from LAN → 403, firmware inventory PATCH from LAN → 403, Redfish
   error envelope on 404s.
9. **Modification round-trips** — boot override PATCH (Once/Pxe) applied,
   visible, restored; serial no-op PATCH accepted; BIOS setting staged at its
   current value and the staged set restored; VirtualMedia insert/eject only
   with `--iso-url` (needs a reachable image).

Data the *managed host* pushes (identity, BIOS attributes, firmware inventory,
sensors) may be legitimately absent on a node whose host never booted: those
checks skip with a reason by default, `--strict` makes them failures.

## Fleet flow (fleet-check.sh)

"Up to date" = node's `/api/application/version` equals this checkout's
`git describe --tags --always` — the same equality rule `pkg/app/autoupdate`
uses, and what `make deploy` stamps.

| conformance | build   | action |
|-------------|---------|--------|
| pass        | current | done |
| pass        | stale   | report (or deploy with `--converge`) |
| fail        | stale   | force-rebuild + `make deploy`, verify version, retest; still failing → diagnose |
| fail        | current | deep-diagnose |

Deploys `rm` the `dist/` binaries first: the Makefile's file targets have no
source dependencies, so a leftover binary from an older commit would otherwise
be shipped as-is. After deploy the runner polls the service root (up to 120 s —
the app self-restarts via SIGTERM/respawn) and confirms the reported version
before retesting.

Diagnosis (`reports/<ts>/<host>.diagnosis/`) captures the full Redfish tree,
`/api/vm/info`, `/api/application/version`, the failed-check list, and a
DIAGNOSIS.md pointing at the relevant handlers (`api/redfish/`).

## Known caveats

- `Managers/1 FirmwareVersion` comes from `debug.ReadBuildInfo()`
  (`api/redfish/managers.go`), not `application.CurrentVersion()`; builds
  without VCS stamping report the fallback `1.0.0`, which section 4 flags.
  The fleet version gate therefore uses the app API, not Redfish.
- First auth on a cold node pays full bcrypt (~1.6 s, single-core RISC-V) and
  failures add a deliberate 2 s delay — the suite budgets generous timeouts.
- Right after a deploy the app respawns; expect a brief window where requests
  fail at the connection level (the runner waits it out).
