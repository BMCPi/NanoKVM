# Host firmware contract

What a managed host's own firmware (EDK2/coreboot, tianocore RedfishPkg +
edk2-redfish-client) must implement to interoperate with this BMC. Companion
to `board-agnostic-design.md` §5-§6 (binding) and the research behind it,
`edk2-redfish-client-gaps.md`. The BMC side of everything here is unchanged
by board — this doc exists so a second board (first target: Intel NUC) can
be brought up without re-deriving it from the RPi's working firmware.

## Update mechanics

The BMC never flashes the host and never pulls an image. It only stages a
UEFI FMP capsule onto a volume the host's own firmware reads, and later
records whatever the host reports back. To be updatable this way, host
firmware must implement all four of:

1. **Scan the capsule volume.** The BMC presents a GPT/ESP FAT32 volume over
   USB mass storage; capsules land in `\EFI\UpdateCapsule\`
   (`pkg/app/firmware/capsule.go:3-6`, UEFI 2.10 §8.5.5). The host decides
   how it notices them — always-scan every boot, or gated on
   `OsIndications` — the BMC does not care which.
2. **A self-flashing FMP driver.** Once a capsule is found, applying it to
   the host's own persistent store (SPI flash under coreboot, for the NUC)
   is entirely the host's FMP driver's job. The BMC has no path to do this
   itself.
3. **Delete-on-apply.** After successfully applying a capsule, delete it
   from `\EFI\UpdateCapsule\`. The BMC's capsule-list view is read fresh off
   the volume on every request specifically because it trusts this signal
   (`pkg/app/firmware/capsule.go:14-16`) — a capsule that is never deleted
   reports as permanently pending, even after a real flash succeeded.
4. **Per-boot `SoftwareInventory` PATCH.** Each boot, PATCH the version and
   `LastAttempt*` fields back to
   `/redfish/v1/UpdateService/FirmwareInventory/<id>`
   (`api/redfish/update_service.go:99-133`). PATCH merges per DSP0266, so a
   boot that omits `LastAttempt*` leaves the last known attempt visible
   rather than clearing it.

Both staging transports (SimpleUpdate fetch-by-URL, and the raw
`HttpPushUri` POST) write the identical volume — host firmware never
touches `UpdateService` itself and does not need to distinguish which
transport an operator used.

### Firmware inventory member ID is host-chosen

The member id under `FirmwareInventory` used to be pinned to `BiosFirmware`
with every other id 404ing; that pin is gone
(`api/redfish/update_service.go:63-97`, `api/redfish/redfish.go:206`). The
BMC serves exactly whatever id(s) the host PATCHes and synthesizes nothing
before the first report — an honest-empty collection, not a fabricated
`BiosFirmware` placeholder. The RPi host continues to report
`BiosFirmware`; a NUC (or any other board) is free to PATCH under its own
id, matching whatever its FMP driver's `ImageIdName`/GUID naturally is.

## USB transport: what the host must drive

The BMC's composite is bounded by silicon, and that bound decides which USB
classes the host firmware has to support. The SG2002's dwc2 core implements
six device IN endpoints (`GHWCFG4.num_dev_in_eps`; the FIFO count in
`/sys/kernel/debug/usb/4340000.usb/fifo` is the same number and the only way
to read it without `/dev/mem`). The composite spends all six:

| function | IN endpoints | |
| --- | --- | --- |
| `mass_storage.disk0` | 1 | the FMP capsule volume, above |
| `eem.usb0` | 1 | the RHI NIC |
| `hid.GS0` | 1 | boot-protocol keyboard |
| `hid.GS1` | 1 | pointer (relative + absolute) |
| `acm.GS0` | 2 | serial console, optional |

### The RHI NIC is CDC-EEM, and stock UsbNetworkPkg will not bind it

`NetworkPkg/UsbNetwork` ships `UsbCdcEcm`, `UsbCdcNcm` and `UsbRndis`. All
three describe a NIC with an interrupt-IN notification endpoint, which costs
two IN endpoints — one more than the budget above leaves once the console is
composed. CDC-EEM has no notification interface at all, so it costs one, and
that is the only reason both a NIC and a CDC-ACM console fit at once.

Consequence for host firmware: **ship a custom EEM SNP driver.** EEM framing
is one 2-byte header per packet (`bmType`, CRC flag, 14-bit length), an
optional Ethernet CRC that may be sent as the `0xdeadbeef` sentinel, and
command packets (echo/echo-response) that a minimal driver may answer or
drop. Two things follow from the missing notification endpoint:

- **No link-state signalling.** ECM/NCM report connect/disconnect over the
  interrupt endpoint; EEM cannot. The driver must assume link-up whenever the
  device is enumerated, and must not wait for a notification that never
  arrives.
- **No speed change notification.** Report a fixed link speed.

A Linux host binds `cdc_eem` on class (02/0C/07) with a wildcard VID/PID, so
the RHI NIC *is* visible to a booted OS. This is a change of policy from the
earlier vendor-specific plan: hiding the NIC from the OS was rejected because
it is not a boundary — `new_id` binds any driver in one line — and the real
control has to live on the BMC side. Note that `CheckAuth` still passes
host-interface requests unauthenticated
(`api/redfish/middleware.go:29-33`), so a booted OS reaching `usb0` reaches
Redfish without credentials. That is unchanged, and unchanged deliberately;
if it ever needs tightening, tighten it there and not in the descriptors.

### The serial console is CDC-ACM

`acm.GS0` is optional and off by default. When on, the host sees a standard
CDC-ACM port (class 02/02) that `cdc_acm` binds with no help, giving a booted
Linux a `/dev/ttyACM*` usable as `console=`. The BMC side is `/dev/ttyGS*`
either way, feeding the web terminal and IPMI SOL through one broker.

EDK2 has no in-tree CDC-ACM `SerialIo` driver — `UsbSerialDxe` in
edk2-platforms is FTDI-specific — so firmware console redirection over this
port needs a custom driver too. It is independent of the NIC: the console is
a convenience, the NIC is the RHI.

## Redfish Host Interface (RHI) conventions

The BMC's design deliberately departs from two things a stock
`edk2-redfish-client` build assumes. Both require a custom platform library
on the host side — using the stock ones will not work against this BMC.

### Credentials: no DSP0270 bootstrap — ship a custom `RedfishPlatformCredentialLib`

The BMC does not implement IPMI Get-Bootstrap-Credentials
(NetFn 0x2C/Cmd 0x02/Group 0x52), has no `AccountService`, and never will
for the host link by design — this is not a gap to fix, it is the chosen
model. Requests arriving over the USB host-interface link pass
`CheckAuth` with no credential check at all
(`api/redfish/middleware.go:24-33`, `IsHostInterfaceRequest`); the trust
boundary is enforced by network isolation instead — the RHI link cannot
reach or be reached by the management LAN
(`pkg/app/network/isolation.go`, nftables + no forwarding + no
router-advertisement acceptance on the link).

Consequence for host firmware: a stock `RedfishCredentialDxe` /
`RedfishPlatformCredentialIpmiLib` will stall forever waiting for bootstrap
credentials that this BMC never issues. Ship a custom
`RedfishPlatformCredentialLib` that returns fixed Basic-auth credentials
(any value works — the BMC accepts whatever arrives over the RHI link
without validating it). The DSP0270 bootstrap stack — `HostInterface`
resource, `CredentialBootstrapping`, disable-control byte — is explicitly
out of scope; if it's ever wanted, it is its own spec, not a patch on this
one.

### MAC/IP pairing: host = BMC + 1, not stock BMC − 1

Stock `PlatformHostInterfaceBmcUsbNicLib`-style conventions derive the
host's address as BMC − 1. This BMC does the opposite:

| | MAC | IP |
| --- | --- | --- |
| BMC (gadget side) | `da:c0:ff:ee:10:01` | `169.254.10.1/16` |
| Host (RHI link) | `da:c0:ff:ee:10:02` | `169.254.10.2` (DHCP lease) |

(`pkg/device/usbgadget/usbgadget.go:47-48`, `pkg/config/default.go:149-150`.)
A custom `RedfishPlatformHostInterfaceLib` must hardcode this topology
(Type 42 USB descriptor + address pair) rather than deriving it from the
stock −1 rule.

## Explicit non-goals (tracked as follow-ups, not bring-up blockers)

None of these block a host from being updatable per the four requirements
above; they are gaps in BMC-side polish that a from-scratch client stack
may notice but that the custom client this BMC targets does not need:

- **SimpleUpdate task monitoring** — `SimpleUpdate` and the `HttpPushUri`
  POST return `202` with a message body only, no `Location` header, no
  `Task` resource. No `TaskService` exists at all; an absent one is logged
  and the host continues.
- **`@Redfish.ActionInfo`** on the `SimpleUpdate` action.
- **`MultipartHttpPushUri` advertisement** — the endpoint already parses a
  multipart body correctly (`streamio.StreamMultipartFile`, never routed
  through `/tmp`), but the `UpdateService` resource advertises only
  `HTTPPushURI`, and `UpdateParameters`/`OperationApplyTime` parts are
  ignored if sent.
- **HTTP `HEAD`** — routes are GET-only; gin does not answer `HEAD` for a
  GET route by default.
