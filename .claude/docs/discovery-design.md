# Discovery (DNS-SD + SSDP) design summary

Condensed from `docs/superpowers/specs/2026-08-30-discovery-dnssd-ssdp-design.md`
(approved 2026-08-30, amended during implementation).

The BMC advertises itself two ways: DNS-SD over mDNS (avahi-browse, Finder) and
SSDP (what DSP0266 §12.4 specifies for Redfish discovery).

## Package layout

- `pkg/protocol/discovery` — lifecycle, config, interface resolution, change
  watcher. The only package that reads config and the only one the app talks
  to. Exports `Start()`, `Restart()`, `Advertised()`. `Restart()` is how
  settings apply (`ui/fragments/settings.go` calls it); responder fields are
  snapshotted at construction.
- `pkg/protocol/discovery/mdns` — DNS-SD responder on `brutella/dnssd`
  (replaced pion/mdns entirely; two responders on 5353 trigger RFC 6762
  conflict renaming).
- `pkg/protocol/discovery/ssdp` — hand-rolled SSDP responder (no library can
  emit the `AL` header or match both Redfish ST variants).

## Invariants

- **Interface scoping is load-bearing.** Answers confined to the configured
  interface (`eth0`); the USB host link (`usb0`, 169.254.10.1) must never
  receive them.
- **Addresses arrive late.** `eth0` may have no address at start; failure to
  start a responder is a warning, not fatal — the watcher retries and restarts
  on hostname/address changes.
- The two responders are independent: one failing to bind must not stop the other.
- Never advertise a service whose subsystem is disabled.
- `Stop()` sends `ssdp:byebye` before closing and is safe to call twice.
- `Advertised()` reports the mDNS name only.

## Services advertised (DNS-SD)

| Service | Port | Gated on |
|---|---|---|
| `_redfish._tcp` | `port.https` (or `port.http` when `proto: http`) | `redfish.enabled` |
| `_https._tcp` | `port.https` | `proto: https` |
| `_http._tcp` | `port.http` | always (the `:80` redirect) |
| `_ssh._tcp` | `ssh.port` | `ssh.enabled` |

`_ipmi._udp` deliberately excluded (no established convention). Redfish TXT:
`txtvers=1`, `protovers=1.0`, `uuid=<BMC UUID>`, `path=/redfish/v1/` — keep minimal.

## SSDP responder

- Enabled only when **both** `discovery.ssdp.enabled` and `redfish.enabled`.
  IPv4 only (`239.255.255.250:1900`, joined on the configured interface via
  `golang.org/x/net/ipv4`); the mdns ipv4/ipv6 toggles do not apply.
- Answers M-SEARCH for `ssdp:all`, `upnp:rootdevice`,
  `urn:dmtf-org:service:redfish-rest:1`, and `...:1:<minor>`; always responds
  with the versioned ST. Honours `MX` (uniform random wait in `[0, min(MX,5))`,
  default 1). Ignores anything that is not `M-SEARCH` + `MAN: "ssdp:discover"`.
- Response carries **both** `AL` and `LOCATION` with the service-root URL —
  DMTF's reference client reads only `AL`; generic UPnP reads only `LOCATION`.
- Sends multicast `NOTIFY ssdp:alive` on start/re-advertise, `ssdp:byebye` on stop.
- `<minor>` comes from `redfishProtocolVersion` in
  `api/redfish/schema_versions.go` (RedfishVersion 1.13.0 → `13`) — never
  hard-code it.

## Config and migration

`discovery:` block replaced the legacy top-level `mdns:`:

```yaml
discovery:
  mdns: {enabled: true, interface: eth0, ipv4: true, ipv6: true, hostname: ""}
  ssdp: {enabled: true, interface: eth0, maxAge: 1800}  # interface empty = inherit mdns's
```

The migration is **forced**: on first boot the legacy block is folded into
`discovery.mdns`, the file is rewritten, and the `mdns:` key is dropped
(`Config.MDNS` is `*MDNS` with `omitempty`). Two details each shipped broken
once and have regression tests:

- The gate is `viper.IsSet("discovery.mdns")`, **not** `viper.IsSet("discovery")`
  — keying on the outer block silently discarded operator settings when only
  `discovery.ssdp:` existed.
- A legacy `mdns: {enabled: false}` must stay disabled; zero-value guards have
  twice revived it as enabled.

Accepted rollback cost: downgrading past the migration reverts mDNS to defaults.

## Shared identity

`pkg/protocol/identity.BMCUUID()` (seeded from the lowest permanent MAC) is the
single UUID used by the SSDP `USN`, the DNS-SD TXT `uuid`, and the Redfish
service root. `api/redfish/identity.go` is a thin wrapper — `pkg` must never
import `api`.
