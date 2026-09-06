# RPi-Specificity Map — nanokvm-app → board-agnostic (NUC: coreboot + EDK2, wired reset line)

## 1. RPi-specific items

| Subsystem | File:Line | What | Difficulty |
|---|---|---|---|
| power/boot | cmd/rpiboot/main.go:36 | Pi 5 BCM2712 BootROM USB payload pusher (VID/PID 0x0a5c:0x2712); manual tool, zero server callers | fundamentally-rpi |
| power/boot | pkg/device/power/power.go:314 | Reset() is only force-off + repower via power button; no code path ever drives a reset line | needs-abstraction |
| power/boot | pkg/config/hardware.go:36 | GPIO roles compiled into 3 NanoKVM profiles; GPIOReset resolved (line 74) but has zero consumers; pin roles unreachable from YAML (`yaml:"-"`) | needs-abstraction |
| power/boot | pkg/config/types.go:268 | Power config has one knob (LegacyMode); no reset-strategy or hold-duration surface | needs-abstraction |
| power/boot | api/redfish/inventory.go:66 | ForceRestart and PowerCycle collapse to one cycle op — same in IPMI ColdReset (chassis.go:67) and UI reset (fragments/power.go:78) | needs-abstraction |
| firmware update | pkg/app/firmware/firmware.go:13 | Capsule contract assumes host always-scans all USB FAT volumes for \EFI\UpdateCapsule + self-flashing FMP; stock EDK2 CoD is OsIndications-gated; deletion is the only "applied" signal | needs-abstraction |
| firmware update | api/redfish/update_service.go:124 | FirmwareInventory/LastAttempt populated only by RpiRedfishSyncDxe per-boot PATCH; "Unknown" forever without an equivalent reporter | needs-abstraction |
| firmware update | api/redfish/odata.go:156 | firmwareBiosMemberID "BiosFirmware" pinned to RPI_REDFISH_FIRMWARE_INVENTORY_ID; other ids 404 on GET | trivial-config |
| firmware update | pkg/app/firmware/virtual_media.go:423 | NoCloud ISO→hybrid-MBR block-device conversion for the Pi boot chain; dead code (no callers) — NUC boots El Torito directly | trivial-config |
| redfish surface | api/redfish/hoststate.go:76 | Entire host inventory (NICs, CPUs, memory, BIOS, boot progress) populated only by RpiRedfishSyncDxe over the RHI; placeholders forever without a ported driver | needs-abstraction |
| redfish surface | api/redfish/bios_catalog.go:29 | Compiled-in BIOS attribute vocabulary transcribed from rpi5-uefi HII formsets; roster test pins it to that firmware tree; NUC attrs degrade to untyped text boxes | needs-abstraction |
| redfish surface | api/redfish/ethernet_interfaces.go:152 | NIC-config↔BIOS-attribute bridge hardcodes EthConfigDxe attribute names | needs-abstraction |
| redfish surface | api/redfish/chassis.go:157 | Fan override Oem.PiBmc.FanOverrideLevel exists to be polled by RpiRedfishSyncDxe → RPI_FAN_PROTOCOL; NUC fans are EC-controlled, knob becomes a silent no-op | needs-abstraction |
| redfish surface | pkg/device/bmcsensor/record.go:1 (+reader.go:20) | Sole host-telemetry channel: OP-TEE pTA on the Pi pushes 32-byte record over RP1 I2C1 into BMC emulated slave-EEPROM (GET_THROTTLED semantics included) | fundamentally-rpi |
| redfish surface | cmd/bmc-sensord/main.go:1 | Host-side (runs on the Pi) OP-TEE pTA client; RP1-BAR handshake; nothing to run on a NUC | fundamentally-rpi |
| redfish surface | pkg/platform/telemetry/host_sensors.go:36 + pkg/protocol/ipmi/hal.go:29 | OTel metrics and IPMI SDR read bmcsensor.Default() directly — no seam for an alternate host-telemetry source | needs-abstraction |
| redfish surface | api/redfish/schema_versions.go:11 | Advertised schema versions pinned to the RPi5 RedfishClientPkg feature-driver builds; mismatch = silent property drops on the client | trivial-config |
| redfish surface | api/redfish/processors.go:46 | Placeholder CPU1 hardcodes ARM / ARM-A64 ("by design an aarch64 Raspberry Pi") — NUC reports as ARM until first host sync | trivial-config |
| redfish surface | api/redfish/systems.go:228 | RHI serving conventions pinned to ComputerSystem v1_5_0 client (and internally inconsistent with the v1_13_0 pin) | trivial-config |
| config | pkg/config/default.go:77 | Serial console default /dev/ttyS1@115200 encodes the Pi UART-header wiring; knob exists, default is Pi-carrier | trivial-config |
| UI/docs | ui/components/overview_host_sensors.go:24 | Temp ceiling 100 °C / throttle color at 80 °C are Pi 5 thermals; a healthy Intel SoC near Tjmax reads as throttling | trivial-config |
| UI/docs | ui/fragments/overview.go:70 | Dead legacy read of sys.Oem["NanoKVM"]["SoC"] no longer populated | trivial-config |
| UI/docs | pkg/protocol/redfish/openapi.yaml:5 | Served docs say "managed Raspberry Pi 5" (also lines 8, 210); plus comment-only Pi references across ~a dozen files (main.go:176, sysinfo/resources.go:6, …) | trivial-config |
| build image | Makefile:102 + .goreleaser.yaml:25 | rpiboot unconditionally built and shipped in every deploy/release archive; no per-host-board packaging axis | trivial-config |
| build image | nanokvm-build …/nanokvm.cfg:316 | CONFIG_I2C_SLAVE_EEPROM + DT node exist only to receive the Pi sensor push; inert but sole host-specific residue in the image | trivial-config |

## 2. Already board-agnostic (deduped)

- **Power mechanics**: ATX-style open-drain button press (300 ms/5.5 s) + power-LED confirmation fits a NUC front panel as-is; BMC-side GPIO addressing (SG2002, /etc/kvm/hw revisions) is NanoKVM-own and carries over, reset line already resolved.
- **USB gadget stack**: HID (incl. BIOS-mode boot HID targeting EDK2 UsbKbDxe — exactly the NUC payload), NCM ethernet, mass storage; VID/PID/UDC all config.
- **RHI plumbing**: usb0/169.254.10.1 NCM link, one-lease DHCP, subnet trust boundary — DSP0270-generic and config-driven.
- **Capsule mechanics**: GPT/ESP/FAT32 volume, stage/list/remove, SimpleUpdate/HttpPushUri transports — pure UEFI 2.10 §8.5.5, no Pi GUIDs; capsule bytes never traverse the RHI.
- **Host-report storage**: hoststate keyed maps, ETag/If-Match, merge/upsert; memory/boot-options/drives/SecureBoot/Bios pending flow — any Redfish client can drive it; honest empties otherwise.
- **Boot-override vocabulary**: standard UEFI targets, UEFI-only mode — fits coreboot+EDK2 exactly.
- **IPMI chassis control** (delegates to power controller), **discovery** (mDNS/SSDP), **identity** (BMCUUID from BMC MAC), **auth/sessions** — BMC-side only.
- **Video capture, HID input, serial console** (fully config-driven), **virtual media** ISO/CD-ROM, **pkg/device/optee** (generic Linux TEE client), **api/vm** dispatch (BMC `reboot` is the BMC's own).
- **Build tree**: RPi boot image/multiconfig/EEPROM seeds already removed; Yocto image builds only cmd/server; no udev rules keyed on RPi USB IDs.

## 3. Decisions the abstraction design must make (unanswered)

**D1 — How do firmware updates reach the NUC's SPI flash?** The BMC stages capsules assuming an always-scan CapsuleOnDisk host (firmware.go:13), but stock EDK2 CoD is OsIndications-gated and scoped to the boot ESP; EDK2-as-coreboot-payload usually cannot reflash coreboot, so a custom FMP/flash driver is implied either way. Does the NUC keep USB-MSD capsule-on-disk (port the always-scan driver + write a coreboot-SPI FMP), or move to a host-pull model over the RHI (new BMC surface — none exists today)? Related: nothing triggers a host reboot after staging, and capsule deletion is the only applied-signal.

**D2 — What is the reset model, and where does it live?** The NUC's reset header is wired to GPIOReset, which is resolved but never driven; Reset() is hardwired to a 5.5 s cold cycle; ForceRestart≡PowerCycle in three dispatch tables (Redfish/IPMI/UI); config has no reset-strategy or pin-role surface. Is reset strategy a config knob, a board profile, or a per-line capability map — and should ForceRestart become the warm reset-line pulse (with AllowableValues, IPMI, and UI advertising what's actually wired) while PowerCycle keeps today's behavior?

**D3 — Where does per-host-platform knowledge come from?** Today it is compiled in and pinned to one firmware tree: BIOS catalog + roster test, EthConfigDxe attribute names, schema-version pins, ARM placeholder CPU, Oem.PiBmc fan knob, "BiosFirmware" member id, Pi thermal thresholds, and the whole telemetry source (OP-TEE/I2C — fundamentally-rpi, only bypassable). Does the design port the RpiRedfishSyncDxe contract verbatim into the NUC EDK2 (BMC unchanged), introduce a loadable per-platform profile (catalog, pins, placeholders, thresholds, telemetry source), or go strictly host-authoritative with honest empties and no fallback vocabulary at all?