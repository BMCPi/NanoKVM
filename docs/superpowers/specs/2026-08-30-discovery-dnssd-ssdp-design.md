# Network discovery: DNS-SD and SSDP

Date: 2026-08-30
Status: approved, not yet implemented

## Problem

The BMC is invisible to discovery tooling. `pkg/mdns` answers A/AAAA queries for
`<hostname>.local` and publishes nothing else — no service records, no SSDP.
An operator who does not already know the address cannot find the box, and
inventory tools that scan for Redfish endpoints walk straight past it.

Two protocols are in scope:

- **DNS-SD** over mDNS, so the BMC's services appear in `avahi-browse`, Finder,
  and anything else browsing `_services._dns-sd._udp`.
- **SSDP**, which is what DSP0266 §12.4 actually specifies for Redfish
  discovery. `_redfish._tcp` is a de-facto convention with no normative DMTF
  binding; SSDP is the standardised path. We do both.

## Constraints inherited from the current implementation

- **Interface scoping is load-bearing.** Answers must be confined to the
  configured interface (`eth0`). The point-to-point USB host link (`usb0`,
  169.254.10.1) must never receive them — the old avahi bbappend enforced the
  same thing with `allow-interfaces=eth0`.
- **Addresses arrive late.** `eth0` may have no address at process start. The
  existing watcher restarts the responder when the hostname or the interface's
  addresses change; anything replacing it needs the same recovery.
- **`Restart()` is how settings apply.** `ui/fragments_settings.go` calls it
  after a settings write. Responder fields are snapshotted at construction, so
  a config change is invisible to a running instance without it.

## Decisions

### 1. Package layout

`pkg/mdns` becomes `pkg/discovery`, with two leaf packages:

```
pkg/discovery/          lifecycle, config, interface resolution, change watcher
pkg/discovery/mdns/     DNS-SD responder (brutella/dnssd)
pkg/discovery/ssdp/     SSDP responder (koron/go-ssdp)
```

The leaf packages own one protocol each and take plain arguments — no
`pkg/config` import, no knowledge of each other. `pkg/discovery` is the only
package that reads config and the only one the rest of the app talks to.

It keeps today's exported surface — `Start()`, `Restart()`, `Advertised()` — so
`api/vm/info.go` and `ui/fragments_settings.go` change only their import path.

**Why one package rather than two peers:** both responders need the same
interface resolution, the same UUID, and the same restart trigger. SSDP needs it
most: its `AL` header embeds the BMC's current address, so an address change
must re-advertise. Two independent packages would duplicate the watcher and
could disagree about what the BMC's address is.

### 2. mDNS library: replace pion/mdns with brutella/dnssd

`github.com/pion/mdns/v2` has no API for PTR/SRV/TXT records and no hook to
inject them. It cannot do this job.

Keeping pion for hostnames *and* adding a DNS-SD library alongside it is worse
than either alone: both bind `224.0.0.251:5353`, both answer A for
`nanokvm.local`, and the DNS-SD side's RFC 6762 §8 conflict probing would read
pion's answers as a name collision and rename the host to `nanokvm-2.local`.

`github.com/brutella/dnssd` v1.2.14 replaces it entirely: one `Responder`, one
socket set, N services, interface selection by name (matching our config
directly), plus probing/conflict detection and a netlink `linkSubscribe()` that
pion lacks.

**Watcher stays.** dnssd caches interface addresses when a service is added and
watches links thereafter, but the hostname override comes from config, which
netlink cannot see. The existing watcher continues to cover hostname changes
and to retry while `eth0` is down; its interval can lengthen now that link
changes are handled natively.

### 3. Services advertised

Instance name is the advertised hostname. Every service is scoped to
`cfg.Interface` and gated on its own subsystem's enable flag — never advertise
something that is not listening.

| Service | Port | Gated on |
|---|---|---|
| `_redfish._tcp` | `port.https`, or `port.http` when `proto: http` | `redfish.enabled` |
| `_https._tcp` | `port.https` | `proto: https` |
| `_http._tcp` | `port.http` | always (the `:80` redirect listener) |
| `_ssh._tcp` | `ssh.port` | `ssh.enabled` |

`_ipmi._udp` is deliberately excluded: IPMI/RMCP has no established DNS-SD
convention, so publishing one would be inventing a private protocol.

`_redfish._tcp` TXT record:

```
txtvers=1
protovers=1.0
uuid=<BMC UUID>
path=/redfish/v1/
```

These keys are convention, not a DMTF table. There is no normative Redfish
DNS-SD binding to copy, which is a reason to keep the record minimal rather
than elaborate.

### 4. SSDP

`github.com/koron/go-ssdp` v0.9.1. Its `Advertise()` returns an advertiser that
answers M-SEARCH automatically and exposes `Alive()` / `Bye()`. Interface
scoping is a process-global `ssdp.Interfaces`, set once at startup — a wart,
but acceptable for a single-responder process.

Wire format is taken from DMTF's own reference implementation
([Redfish-Mockup-Server/rfSsdpServer.py][mockup]), not from prose. Answer
M-SEARCH on `239.255.255.250:1900` for these search targets:

- `ssdp:all`
- `upnp:rootdevice`
- `urn:dmtf-org:service:redfish-rest:1`
- `urn:dmtf-org:service:redfish-rest:1:<minor>`

Response:

```
HTTP/1.1 200 OK
CACHE-CONTROL: max-age=1800
ST: urn:dmtf-org:service:redfish-rest:1:<minor>
USN: uuid:<UUID>::urn:dmtf-org:service:redfish-rest:1:<minor>
AL: <service root URL>
EXT:
```

`<minor>` is the minor component of the service root's `RedfishVersion`, which
is `1.13.0`, so `<minor>` is `13`. That value lives in
`api/redfish/schema_versions.go` as `redfishProtocolVersion`; the SSDP
responder must read the same source rather than hard-coding `13`.

Beyond the DMTF reference we also send multicast `NOTIFY ssdp:alive` on start
and on re-advertise, and `ssdp:byebye` on shutdown. DSP0266 says *should*; the
mockup server implements neither. Without alive, the BMC stays invisible until
something actively searches.

The whole SSDP responder requires **both** `discovery.ssdp.enabled` and
`redfish.enabled`. Its search target is Redfish-specific and means nothing
otherwise, so `redfish.enabled: false` disables SSDP no matter what the
discovery block says.

SSDP is IPv4-only. `239.255.255.250:1900` is the only group we join; the
`discovery.mdns.ipv4` / `ipv6` toggles apply to the DNS-SD responder alone.
UPnP's IPv6 groups exist but nothing in this fleet searches on them, and
go-ssdp does not support them.

[mockup]: https://github.com/DMTF/Redfish-Mockup-Server/blob/main/rfSsdpServer.py

### 5. Configuration

New `discovery:` block replacing the top-level `mdns:`:

```yaml
discovery:
  mdns:
    enabled: true
    interface: eth0
    ipv4: true
    ipv6: true
    hostname: ""          # empty = OS hostname
  ssdp:
    enabled: true
    interface: eth0       # empty = inherit discovery.mdns.interface
    maxAge: 1800
```

`checkDefaultValue` migrates a legacy top-level `mdns:` block into
`discovery.mdns` when `discovery:` is absent, so an existing
`/etc/kvm/server.yaml` keeps working untouched and unedited. The migration is
one-way and does not rewrite the file.

### 6. Shared identity

The SSDP `USN` and the DNS-SD TXT `uuid` must be the same UUID the Redfish
service root reports. Today that is `managerUUID()`, unexported in
`api/redfish/identity.go`. `pkg/discovery` importing `api/redfish` inverts the
layering (`api` depends on `pkg`, not the reverse).

Move it to `pkg/identity`, exporting `BMCUUID()`. The identity seed needs only
the lowest permanent MAC, so `pkg/identity` gets a small MAC enumerator rather
than a move of `listManagerNICs`, which stays in `api/redfish` serving the
Redfish NIC inventory. `api/redfish/identity.go` becomes a thin call into
`pkg/identity`, and `identity_test.go` moves with the logic.

## Error handling

- Neither responder failing to start is fatal. Today's behaviour holds: log a
  warning and let the watcher retry, because the usual cause is an interface
  that is not up yet.
- The two responders are independent. SSDP failing to bind `:1900` must not
  stop DNS-SD from serving, and vice versa.
- `Advertised()` continues to report the mDNS name only, since that is what the
  vm-info endpoint and the settings page display.
- `Stop()` must send `ssdp:byebye` before closing, and must stay safe to call
  more than once.

## Testing

Unit, on the pure parts:

- `localNames` normalisation (existing test moves across).
- Service-list construction from a config fixture: the enable-flag gating and
  port selection in §3, including `proto: http` moving `_redfish._tcp` to the
  HTTP port.
- TXT assembly, including that `uuid` matches `pkg/identity.BMCUUID()`.
- SSDP response formatting against literal expected bytes, including that
  `<minor>` tracks `redfishProtocolVersion` rather than a hard-coded 13.
- Config migration: a legacy `mdns:` block lands in `discovery.mdns`, and an
  explicit `discovery:` block wins over a legacy one.

Integration: bring both responders up on the loopback interface and, in the
same process, browse for the service types and send an M-SEARCH, asserting on
what comes back.

## Rejected alternatives

- **Keep pion, add a DNS-SD library alongside.** Two responders on
  `224.0.0.251:5353` answering for the same name; conflict probing renames the
  host. See §2.
- **Hand-roll DNS-SD records on top of pion.** No injection point; would need a
  fork.
- **Hand-roll SSDP.** ~150 lines of HTTPU plus a timer, against a maintained
  library that already answers M-SEARCH and does alive/byebye.
- **Keep everything under the `mdns:` config key.** Least churn, but the name
  would lie about half of what the block configures.
- **Advertise `_ipmi._udp`.** No established convention to follow. See §3.

## Out of scope

- Redfish `EventService` / subscription-based push. Unrelated to discovery.
- Unicast DNS-SD registration (RFC 6763 §11) against a site DNS server.
- Changing `RedfishVersion` further. It was corrected to `1.13.0` separately;
  this design only consumes it.
