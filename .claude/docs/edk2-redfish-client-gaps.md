# BMC Gap Analysis: nanokvm-app vs edk2 RedfishPkg / edk2-redfish-client Requirements

**Context found in code:** the host firmware is a *custom* sync client (`RpiRedfishSyncDxe`, per `api/redfish/update_service.go:123`), and `CheckAuth` deliberately passes host-interface requests through **unauthenticated** (`api/redfish/middleware.go:29-33`, gated by `IsHostInterfaceRequest` + nftables isolation in `pkg/app/network/isolation.go`). That design choice substitutes for the entire DSP0270 credential-bootstrapping stack today — fine for the custom client, fatal for stock `RedfishCredentialDxe`, which will not proceed without IPMI-issued Basic-auth credentials.

## Mandatory requirements

| Requirement | Status | Evidence |
| --- | --- | --- |
| IPMI Get Bootstrap Account Credentials (NetFn 0x2C/Cmd 0x02/Group 0x52) | **MISSING** | No 0x2C/GroupExt handler in `pkg/protocol/ipmi/` (grep: none); registry supports arbitrary NetFn (`go-ipmi handlers/registry.go:83` shows OEM NetFn pattern), so it is registerable |
| Bootstrap account creation in ManagerAccount collection (unique user, random pw, ≤16 chars) | **MISSING** | No `/redfish/v1/AccountService` routes at all (`api/redfish/redfish.go:45-214`); `pkg/app/auth` is single-operator-account |
| Bootstrap creds host-interface-only; 0x2C rejected on eth0 | **MISSING** | `udp.Listen(":port")` binds all interfaces (`pkg/protocol/ipmi/ipmi.go:132`), no per-interface confinement; moot until 0x2C exists |
| Delete bootstrap accounts on BMC/host reset; account can GET Accounts + DELETE itself | **MISSING** | No account resources; host-reset hook exists (`api/redfish/hoststate.go` state tracker) but drives nothing account-shaped |
| HostInterface resource + CredentialBootstrapping{Enabled,EnableAfterReset,RoleId} | **MISSING** | No `Managers/1/HostInterfaces` route (`redfish.go:172-183`) |
| Get Manager Certificate Fingerprint (0x2C/0x01, SHA-256 of TLS cert) | **MISSING** | No handler; TLS cert exists (`cmd/server/main.go:368` `ensureServerCert`) |
| HTTPS on TCP 443 reachable on usb0 | **SATISFIED** | `pkg/config/default.go:24-27` (Proto https, HTTPS 443); `cmd/server/main.go:391` `ListenAndServeTLS` binds all interfaces incl. usb0 |
| HTTP Basic auth with bootstrap creds, account valid whole boot | **PARTIAL** | Basic auth works for operator account (`middleware.go:38-45`); host interface bypasses auth entirely — stock client sending bootstrap Basic creds would actually pass (any creds pass on usb0), but only because auth is skipped, not validated |
| Type 42 descriptor: stable gadget idVendor/idProduct/serial + agreed MAC | **SATISFIED** (BMC side) | Nothing authors Type 42 in the BMC — correct, the *host* authors it; MAC pair is a fixed contract with the host's EDK2 UsbNetworkPkg (`pkg/device/usbgadget/usbgadget.go:43-48`) |
| Redfish-over-IP record: service UUID = service-root UUID | **SATISFIED** (BMC side) | `pkg/identity.BMCUUID()` single-UUID rule; service root emits it (`api/redfish/service_root.go:22`, `identity.go`) |
| Keep serving DHCP on usb0, lease matching advertised convention | **SATISFIED** | `pkg/app/network/rhidhcp.go:33` (server 169.254.10.1, lease .2, 1h); note convention is host=BMC**+1** — see conditional row below |
| GET /redfish → {"v1": "/redfish/v1/"} | **SATISFIED** | `api/redfish/service_root.go:39-43` |
| Fully versioned @odata.type ≥ edk2 driver versions | **SATISFIED** | `api/redfish/schema_versions.go:44-49`: ComputerSystem v1_13_0, Bios v1_1_0, AttributeRegistry v1_3_6, BootOption v1_0_4, SecureBoot v1_1_0, Memory v1_7_1 — exact matches |
| Systems collection with member whose UUID = host SMBIOS UUID; stable URIs | **PARTIAL** | `/Systems/1` stable; UUID is host-PATCHed (`api/redfish/systems.go:100,134,246`) so empty before first-ever report; no POST on /Systems, so stock collection-driver fallback (CreateCollectionResource) would 404 |
| Pre-existing PATCHable Boot object, Bios link, Bios/Settings | **SATISFIED** | Routes `redfish.go:88-105`; `api/redfish/bios.go:66-72` @Redfish.Settings annotation; DELETE of Settings (`redfish.go:105`) clears the stage — anti-reboot-loop |
| @Redfish.Settings with deterministic ETag on SettingsObject | **SATISFIED** | `api/redfish/resources.go:331-340`; `hostETag` = hash of body (`hoststate.go:384-425`), stable per content |
| HTTP semantics: 200/201/202, no gzip unless asked, no transient 500s, non-blocking handlers | **SATISFIED** | Gin/net/http defaults; `HostTrace` is log-only (`middleware.go:52-73`); GETs render from in-memory host state |
| Fresh state on every GET (client caches, expires itself) | **SATISFIED** | All host resources render live from `hoststate` store (`hostreports.go:232-243`) |
| Honor disable-control byte → Enabled=false, 0x80 thereafter | **MISSING** | Depends on 0x2C + CredentialBootstrapping, both absent |
| IPMI transport for 0x2C reachable by NUC firmware | **PARTIAL** | RMCP+ UDP server exists and answers on usb0 (`ipmi.go:132-137`), but requires session auth with configured user; custom IpmiLib backend must know those creds; no group-ext dispatch |
| SimpleUpdate accepts ImageURI/TransferProtocol/Targets (+ ActionInfo) | **PARTIAL** | ImageURI/TransferProtocol/Targets bound (`update_service.go:161-165`); no Username/Password, no @Redfish.ActionInfo |
| 202 + Location task-monitor + Task resource for long updates | **PARTIAL** | Returns 202 with a Message body only, no Location header, no Task (`update_service.go:191-197`); no TaskService anywhere |
| BMC determines/report update outcome itself | **SATISFIED** | Host PATCHes SoftwareInventory "BiosFirmware" (version + ESRT/FMP data) each boot (`update_service.go:122-156`) |
| Do NOT expect host to pull UpdateService/ImageURI | **SATISFIED** | Architecture is exactly capsule-staging (`update_service.go:1-15`); host applies at next boot via FMP |

## Conditional / optional requirements

| Requirement | Status | Evidence |
| --- | --- | --- |
| Get Channel Info + Get LAN Config Params for usb0 (stock BmcUsbNicLib feed) | **PARTIAL** | go-ipmi v0.9.1 framework registers both (`handlers/app.go:36,133`, `handlers/transport.go:29`), but `appHAL.Network()` returns nil (`pkg/protocol/ipmi/hal.go:59`) → address params answer "not supported" |
| Stock-lib MAC/IP convention (host = BMC − 1) | **VIOLATED** (as research predicted) | Host `:02`/`.2`, BMC `:01`/`.1` — host = BMC **+1** (`usbgadget.go:47-48`, `default.go:146-150`); custom RedfishPlatformHostInterfaceLib must encode real topology |
| ETag on GET (header + @odata.etag), If-Match honored | **SATISFIED** | `hostreports.go:232-243` (ETag header, If-None-Match 304, @odata.etag), `hoststate.go:412-425` (If-Match 412) — better than the PCD fallback needs |
| AttributeRegistry: Registries collection + PUT at Location Uri | **SATISFIED** | `redfish.go:111-117` (GET/PUT `:registry` wildcard for EDK2's derived URI, plus /Registries collection) |
| BootOptions POST/DELETE; Memory collection POST; SecureBoot PATCH | **SATISFIED** | `redfish.go:121-139` |
| TaskService (BMC→host task dispatch) | **MISSING** | No route, no resource — acceptable: absent TaskService logs and continues on the host |
| TLS cert enrollable/verified (TlsCaCertificate) | **PARTIAL** | Self-signed cert generated (`main.go:363-368`); works unverified with EDK2 HttpDxe; no stable published CA story |
| MultipartHttpPushUri advertised + UpdateParameters part parsed | **PARTIAL** | `/UpdateService/update` streams multipart `UpdateFile` correctly off-tmpfs (`update_service.go:214-226`) but resource advertises only `HTTPPushURI` (`update_service.go:54`) and ignores UpdateParameters/OperationApplyTime |
| HTTP HEAD support | **LIKELY MISSING** | Routes registered as GET only; gin does not answer HEAD for GET routes by default (unverified at runtime) |

## Top gaps ranked by NUC bring-up blocking severity

1. **IPMI credential bootstrapping (0x2C/0x02/0x52) + AccountService** — the only hard blocker *if* the NUC build uses stock `RedfishCredentialDxe`/`RedfishPlatformCredentialIpmiLib`: no credentials → no client traffic at all. Escape hatch already shipped: unauthenticated host-interface passthrough means a custom credential lib returning any Basic creds works today. Decide which path the NUC EDK2 takes before writing code.
2. **NetworkHAL is nil** — stock `PlatformHostInterfaceBmcUsbNicLib` cannot build Type 42 (LAN params answer "not supported"), and the MAC/IP convention is inverted (host=BMC+1 vs stock's −1). Either implement a usb0-backed `hal.NetworkHAL` *and* flip the convention, or (cheaper, consistent with the existing custom-firmware contract) hardcode topology in a custom `RedfishPlatformHostInterfaceLib`.
3. **HostInterface resource + CredentialBootstrapping object** — required by DSP0270 once bootstrapping exists; also where `EnableAfterReset`/disable semantics live. Meaningless until (1) is decided.
4. **ComputerSystem UUID empty before first host report** — stock identify fails on a virgin BMC and the client falls back to POST /Systems (405 today). Seed UUID from host-state/Type-42 knowledge, or accept first-boot no-sync.
5. **SimpleUpdate task monitoring** — 202 without Location/Task violates DSP2062 client expectations (operator tooling, not the EDK2 host client, which never touches UpdateService). Low for bring-up.
6. **Get Manager Certificate Fingerprint (0x2C/0x01)** — spec-mandatory but consumed by nothing yet; implement alongside (1) for ~free.
7. **HEAD + MultipartHttpPushUri advertisement + ActionInfo** — polish; nothing in the boot path reads them.
