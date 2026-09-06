# NUC Readiness Verdict — nanokvm-app @ HEAD (338a36d)

## 1. Headline

**Yes — this firmware can control an Intel NUC today.** Power, reset, video, HID, virtual media, serial, IPMI, and discovery work with wiring plus two config keys; nothing at HEAD assumes a Raspberry Pi host on any runtime path. The honest qualifier: **full Redfish management (inventory, BIOS config, firmware update) is gated on a custom coreboot+EDK2 payload** that must ship three things the stock tree lacks — the UsbCdcNcm driver in its DSC/FDF (without it the Redfish Host Interface never enumerates), two custom platform libs (fixed-credential + host=BMC+1), and the four-duty capsule contract. Also, "wire two pins" undersells power: **LED sense is load-bearing and fails silently if miswired**, and one real BMC defect remains (Ethernet host-state lost on restart).

## 2. Still Pi-specific at HEAD (real NUC impact)

| Area | Evidence | NUC impact |
| --- | --- | --- |
| Redfish Sensors + Chassis/Thermal bypass the hostsensor seam, direct-reading bmcsensor | api/redfish/sensors.go:51, api/redfish/chassis.go:97 | Degrades gracefully (empty, not fake), but any future non-RPi Source would never surface in Redfish Sensors/Thermal — the two acknowledged deferred seam consumers |
| New seam's vocabulary mirrors RPi throttle bits; Prometheus help text says "as reported by OP-TEE over I2C" | pkg/device/hostsensor/hostsensor.go:28-29; pkg/platform/telemetry/host_sensors.go:41 | Cosmetic-but-visible: misleading metric help on a transport-neutral seam; conditions (UnderVoltage/FreqCapped…) reusable but Pi-flavored |

Cosmetic residue, inert at runtime: dead SaveMediaISO conversion chain (pkg/app/firmware/virtual_media.go:423, zero callers), dead `Oem["NanoKVM"]` read (ui/fragments/overview.go:68-93), pkg/device/optee living under pkg/ but linked only into bmc-sensord, stale v1_5_0 comment at api/redfish/systems.go:234-241, comment-only Pi mentions, and the kernel I2C-slave-EEPROM config in nanokvm-build (empty EEPROM, seam reports absence).

## 3. Function-by-function NUC control matrix

| Function | Verdict | Detail (verifier-corrected) |
| --- | --- | --- |
| Power on/off/status | **Works-as-is, with wiring caveats** | Button presses on GPIOPower + LED-sense state (power.go:184-327). Correction: PowerOn/PowerOff/ForceOff *gate* on LED sense — a configured-but-unwired or wrong-level PWR_LED line reads garbage silently, turning ops into wrong-direction no-ops with no error. Mind 5V front-panel LED vs SG2002 3.3V GPIO; a standby-blinking LED passes the 10 ms debounce and can flap Watch / spuriously satisfy waitForOff |
| Reset | **Works-as-is** | Default `power.reset: auto` → 250 ms open-drain active-low pulse on GPIOReset (power.go:396) — physically correct for a RESET# header. Redfish ForceRestart, IPMI hard reset, UI Reset all dispatch it; PowerCycle stays force-off+repower. `line` errors on unwired boards |
| Video capture | **Works-as-is for firmware, imperfect EDID** | FSP-GOP/libgfxinit/EDK2 GOP pick the preferred 1080p60 DTD — fine. Correction: the EDID also advertises modes the cv181x pipeline cannot capture (720x400@70 < 480-line min, interlaced 1080i50, 1280x1024@60/75, 170 MHz range-limit vs the driver's 150 MHz cap) — an OS driver could pick a GTF mode (e.g. 1600x1200@60 ~161 MHz) that outruns capture |
| HID keyboard/mouse | **Works-as-is for keyboard; pointer caveat** | Strict 8-byte boot keyboard, subclass=1 (BIOSMode default true) — EDK2 UsbKbDxe binds; setup fully navigable. Correction: BIOSMode writes subclass=1 to *both* HID functions; EDK2 mouse drivers will bind the pointer and misparse its report-ID-prefixed 7-byte reports — possible spurious pre-boot pointer events. Pre-boot pointing not expected anyway |
| Serial console | **With-config** | Keys: `serial.device`, `serial.baudRate`, `serial.parity`, `serial.dataBits`, `serial.stopBits`, `serial.flowControl`, `serial.capture.enabled`, `console.primaryView`. Wired COM header: match EDK2 terminal PCDs. No serial: `console.primaryView: hdmi`, `serial.capture.enabled: false` |
| Virtual media | **Works-as-is** | Hybrid ISO (0xAA55 MBR) → cdrom=0 disk; pure ISO → cdrom=1 El Torito; EDK2 boots both natively; no arch assumption, no /tmp overlay use |
| Firmware update | **Needs-host-side** | BMC staging complete (GPT/ESP FAT32, \EFI\UpdateCapsule\). EDK2 payload must: scan attached FAT volumes for capsules, ship a coreboot-SPI FMP flashing driver, delete-on-apply (only "applied" signal), PATCH its SoftwareInventory per boot. Nothing triggers a host reboot after staging |
| Redfish surface / edk2-redfish-client | **Needs-host-side (three items)** | BMC surface complete (PUT-able registry, keyed hoststate, ETags/If-Match, honest empties, arch-neutral CPU1). Host must build: (1) **UsbCdcNcm from UsbNetworkPkg into the DSC/FDF — gadget is NCM-only, no ECM/RNDIS fallback, and no platform builds it by default**; (2) custom RedfishPlatformCredentialLib (fixed Basic creds — no DSP0270 bootstrap exists); (3) custom RedfishPlatformHostInterfaceLib (host da:c0:ff:ee:10:02/169.254.10.2, Type 42 → 169.254.10.1). Correction: first-boot stock-identify fallback POST /Systems gets **404** (no route, HandleMethodNotAllowed unset), not 405 |
| IPMI | **Works-as-is** | Operator convenience over RMCP+ (creds `ipmi.username`/`password`); hard reset uses the reset line; SDR honestly empty; no 0x2C bootstrap by design |
| Host sensors/telemetry | **With-config** | No Source registers on a NUC; UI/IPMI/OTel/Redfish all render honest absence. Set **`hardware.fanControl: false`** (profile default true) so Chassis stops advertising the inert Oem.PiBmc.FanOverrideLevel knob |
| Discovery/identity | **Works-as-is** | DNS-SD + DSP0266 SSDP on eth0, single BMC UUID, interface-scoped away from usb0. Client finds the BMC via Type 42, not discovery |

**BMC gap (verified defect):** LoadHostState (api/redfish/hoststate.go:327-337) restores BootOptions/Memory/Processors/Drives/Firmware but **omits host.Ethernet** — host NIC inventory and operator-staged EthernetInterface PATCHes (DHCPv4, IPv4StaticAddresses, StaticNameServers) are silently lost on BMC restart until the host re-POSTs.

## 4. Bring-up sequence

1. **Build/package BMC**: `make app` + deploy with `BOARD_TOOLS=` (drops rpiboot + bmc-sensord from the archive).
2. **BMC config**: `hardware.fanControl: false`; serial keys per wiring (or `console.primaryView: hdmi` + `serial.capture.enabled: false` if none); leave `power.reset: auto`.
3. **Physical wiring**: front-panel PWR_BTN and RESET# to GPIOPower/GPIOReset; PWR_LED to GPIOPowerLED **with level-shift/series resistor if the header is 5V, and verify polarity with a meter — power ops gate on this line and misreads are silent**; NUC HDMI out → LT6911 in; NUC USB → NanoKVM USB-C gadget port; eth0 → management LAN.
4. **EDK2/coreboot payload build** (the long pole): add UsbCdcNcm (UsbNetworkPkg) to DSC/FDF; implement RedfishPlatformCredentialLib (fixed Basic creds) and RedfishPlatformHostInterfaceLib (host=BMC+1 addressing, Type 42 at 169.254.10.1); implement the four capsule-contract duties per .claude/docs/host-firmware-contract.md; build RedfishClientPkg feature drivers at (or compatible with) the pinned schema versions in api/redfish/schema_versions.go.
5. **Validate in order**: power state tracks LED (on/off/force-off, watch for standby-blink flapping) → warm reset pulses RESET# → video shows 1080p60 in firmware setup → keyboard navigates setup → ISO insert boots → NCM enumerates and client syncs (Systems/Bios/registry populate; expect UUID empty + 404-fallback quirk on very first boot) → SimpleUpdate round-trip (capsule staged → host flashes → deletes → PATCHes inventory).

## 5. Residual BMC work worth doing (ranked)

1. **Fix host.Ethernet persistence** in LoadHostState — real latent defect breaking the EthernetInterface feature-driver flow across BMC restarts.
2. **Harden LED-sense**: sanity-check/warn on implausible readings, and reconsider the 10 ms debounce vs standby-blink (spurious waitForOff satisfaction mid-Reset).
3. **Route Redfish Sensors + Chassis/Thermal through the hostsensor seam** — the two acknowledged deferred consumers; prerequisite for any future NUC telemetry Source appearing in Redfish.
4. **Trim the EDID** to the capture envelope: drop 720x400/1280x1024/1080i50, lower the range-limit max dotclock to ≤150 MHz so OS GTF modes can't outrun capture.
5. **Scope BIOSMode subclass=1 to the keyboard function only**, avoiding EDK2 mouse drivers binding and misparsing the report-ID-prefixed pointer.
6. **Cosmetic sweep**: neutralize the OP-TEE/I2C metric help text and Pi-flavored condition names; delete the dead SaveMediaISO chain and `Oem["NanoKVM"]` read; fix the stale v1_5_0 comment (systems.go:234-241); relocate pkg/device/optee toward cmd/bmc-sensord.
