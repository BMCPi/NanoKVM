# Board-agnostic host support — design

Approved 2026-08-31. First non-RPi target: Intel NUC, coreboot with custom
EDK2 payload, host firmware running tianocore RedfishPkg +
edk2-redfish-client over the Redfish Host Interface (usb0). The NUC's reset
button header is wired to the NanoKVM's reset pin.

Research inputs (kept alongside this spec): `rpi-specificity-map.md` (what
is RPi-specific, with file:line evidence) and `edk2-redfish-client-gaps.md`
(BMC-side requirements of the tianocore client stack vs what exists).

## Decisions this design rests on

- **Credentials: custom lib, not DSP0270 bootstrap.** The NUC's EDK2 ships a
  `RedfishPlatformCredentialLib` returning fixed credentials; the BMC keeps
  the deliberately isolated, unauthenticated host-interface design (nftables
  guard + subnet trust boundary). The DSP0270 stack (IPMI 0x2C group 0x52
  bootstrap accounts, AccountService, HostInterface resource) is explicitly
  OUT of scope; if it is ever wanted, it is its own spec.
- **Platform vocabulary is host-authoritative.** The EDK2 registry reporting
  fix means the host supplies the complete `BiosAttributeRegistry`
  (verified live: 30 attributes, typed, full enum value lists with current
  and default values, via PUT at the registry URI the BMC already accepts).
  The BMC never needs a compiled-in per-board vocabulary again.
- **Update mechanics do not change.** Capsule staging (GPT/ESP/FAT32 volume,
  SimpleUpdate + HttpPushUri transports) is pure UEFI 2.10 and stays. How a
  capsule reaches SPI flash on the NUC is the host firmware's FMP driver's
  job. The client stack never pulls from UpdateService; it reports
  SoftwareInventory back. Per-board firmware requirements (always-scan
  CapsuleOnDisk or OsIndications, self-flashing FMP) become documentation,
  not code.

## 1. Reset model

`pkg/device/power`:

- New `(*Controller).ResetLine(ctx) error`: momentary pulse of the
  `GPIOReset` line the hardware profiles already resolve (same gpiocdev
  request/hold/release discipline as the power button; ~250 ms active,
  active-low per the profile). Errors when the profile wires no reset pin.
- `(*Controller).CanResetLine() bool` reports wiring, from the resolved
  profile.

Config (`pkg/config`): the `power` block gains

```yaml
power:
  reset: auto   # auto | line | cycle
```

- `auto` (default): use the reset line when wired, else force-off+repower.
- `line`: reset line only; Reset actions error if unwired (operator asked
  for something the hardware cannot do — do not silently substitute a
  power cycle, which is destructive to the host OS).
- `cycle`: today's behavior always, even when the line is wired.

Dispatch — all three tables change consistently:

| Action | line available (auto/line) | cycle (or unwired auto) |
| --- | --- | --- |
| Redfish `ForceRestart` | reset-line pulse | force-off + repower (today) |
| Redfish `PowerCycle` | force-off + repower (always) | same |
| IPMI chassis control `power cycle` | force-off + repower | same |
| IPMI chassis control `hard reset` | reset-line pulse | force-off + repower |
| UI reset control | follows ForceRestart | same |

`ResetType@Redfish.AllowableValues` always lists both `ForceRestart` and
`PowerCycle`; the table above defines what each does on the given wiring.
The UI labels the reset control "Reset" vs "Power cycle" according to what
it will actually do.

## 2. Host-authoritative BIOS vocabulary

- Delete `api/redfish/bios_catalog.go` and its roster test. The registry
  served at `/redfish/v1/Registries/...` and
  `/Systems/1/Bios/BiosAttributeRegistry` is exactly what the host last
  PUT; before first sync it renders an honest empty registry (valid
  AttributeRegistry with zero attributes), never a fabricated one.
- Attribute-typed rendering anywhere in the UI/API reads the host-supplied
  registry. Unknown attributes (host reported a value the registry lacks)
  degrade to untyped display, as today.
- The `EthConfigDxe` NIC↔BIOS bridge (`api/redfish/ethernet_interfaces.go`)
  activates only when the live registry contains the attribute names it
  bridges; otherwise the bridge is inert and NIC settings PATCHes that
  would need it are rejected with a clear extended-info message.

## 3. Conditional RPi surfaces

- `Oem.PiBmc.FanOverrideLevel` (Chassis): present only when
  `hardware.fanControl: true` (new knob; default true on the RPi hardware
  profiles, false otherwise). Absent property, not silent no-op.
- Firmware inventory member ID: serve whatever member IDs the host PATCHes
  into SoftwareInventory; drop the pinned `BiosFirmware` 404-others rule.
  The RPi host continues to report `BiosFirmware`; the NUC reports its own.
- Placeholder ComputerSystem/Processor values before first host sync become
  architecture-neutral (no ARM/A64 claim); UUID stays empty until reported
  (accepted: stock-client first-boot identify quirk is not our client).
- Overview thermal thresholds (100 °C ceiling / 80 °C throttle color) move
  to per-source values supplied by the telemetry seam (§4); the RPi source
  keeps today's numbers.
- `pkg/protocol/redfish/openapi.yaml` and UI copy lose "Raspberry Pi"
  wording (service describes "the managed host").
- Serial console default stays `/dev/ttyS1@115200` (config already covers
  other wiring).

## 4. Host-telemetry seam

New small interface in `pkg/device/hostsensor`; `pkg/device/bmcsensor`
becomes its RPi implementation:

```go
type Source interface {
    Latest() (Reading, bool)   // false: no host telemetry available
    Thresholds() Thresholds    // display/alert values for this source
}
```

- Consumers migrate to the seam: `pkg/platform/telemetry` host-sensor
  metrics, IPMI SDR population, overview UI sensors.
- RPi: the existing OP-TEE/I²C `bmcsensor` chain implements `Source`
  unchanged. `cmd/bmc-sensord` remains RPi-only.
- NUC: no Source registered → consumers render/report absence honestly
  (no fabricated sensors). Host-reported Redfish data (if its client ever
  reports thermal) is a future Source, not built now.

## 5. Update path (documentation, not code)

- Capsule staging, both transports, stage/list/remove semantics: unchanged.
- A new doc section (in this file's companion `host-firmware-contract.md`,
  written during implementation) records what a host firmware must
  implement to be updatable this way: scan the staged volume for
  `\EFI\UpdateCapsule` (always-scan or OsIndications — board's choice),
  FMP driver that can flash its own store (SPI under coreboot for the NUC),
  delete-on-apply signal, and per-boot SoftwareInventory PATCH for
  version/LastAttempt reporting.
- Explicit non-goals now (tracked as follow-ups, not blocking bring-up):
  SimpleUpdate task monitor + Location header, `@Redfish.ActionInfo`,
  MultipartHttpPushUri advertisement, HEAD support.

## 6. Packaging

- `rpiboot` and `bmc-sensord` stop shipping unconditionally: a
  `BOARD_TOOLS` axis in the Makefile / goreleaser config (default keeps
  today's contents for RPi deploys; NUC deploys exclude both).
- No image-side changes required for bring-up (the I²C slave-EEPROM kernel
  residue is inert on a NUC host).

## Testing

- Unit: reset dispatch across all three tables × {line wired, unwired} ×
  {auto, line, cycle}; `line`+unwired errors. Registry precedence: host PUT
  wins, honest-empty before sync, catalog deletion leaves no fallback path.
  Config migration: absent `power.reset` reads as `auto`; existing configs
  unchanged. Fan knob presence/absence in Chassis rendering.
- On-device: RPi regression smoke (with `reset: auto` and the RPi wiring,
  behavior is unchanged; the catalog-less BIOS page renders correctly
  against the fixed EDK2), then NUC bring-up: reset-line pulse observed,
  client sync populates registry/inventory, capsule staged and applied by
  the custom FMP.

## Out of scope

- DSP0270 credential bootstrapping stack (own spec if wanted).
- Any NUC-side telemetry agent.
- TaskService.
- Board-profile file format (rejected: host-authoritative model).
