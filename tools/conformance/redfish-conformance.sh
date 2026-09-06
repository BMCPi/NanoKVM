#!/usr/bin/env bash
# redfish-conformance.sh — the standard Redfish test list every NanoKVM BMC
# node must pass. Run from a workstation against a live node; used standalone
# or per-host by fleet-check.sh to find non-conforming nodes.
#
# Usage: redfish-conformance.sh [options] <host>
#   -u USER:PASS     credentials (default admin:admin, or RF_USER/RF_PASS env)
#   --read-only      skip the mutation round-trips (boot override, serial, bios)
#   --strict         host-pushed data (BIOS attrs, sensors) missing = FAIL, not skip
#   --iso-url URL    also exercise VirtualMedia InsertMedia/EjectMedia with URL
#
# Every mutation restores the state it found. Exit 0 iff no check failed.
set -uo pipefail

TOOL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$TOOL_DIR/lib.sh"

die() { echo "redfish-conformance: error: $*" >&2; exit 2; }
usage() { sed -n '2,12p' "${BASH_SOURCE[0]}"; exit 2; }

HOST="" MUTATE=1 STRICT=0 ISO_URL=""
while (($#)); do
  case "$1" in
  -u) [[ "$2" == *:* ]] || die "-u wants USER:PASS"; RF_USER="${2%%:*}" RF_PASS="${2#*:}"; shift ;;
  --read-only) MUTATE=0 ;;
  --strict) STRICT=1 ;;
  --iso-url) ISO_URL="$2"; shift ;;
  -h | --help) usage ;;
  -*) die "unknown flag $1" ;;
  *) [[ -n "$HOST" ]] && die "one host only"; HOST="$1" ;;
  esac
  shift
done
[[ -n "$HOST" ]] || usage
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"

RF_BASE="https://$HOST"

echo "Redfish conformance: $HOST (user $RF_USER)"

# Data the managed host pushes (BIOS attrs, firmware inventory, sensors) may
# legitimately be absent on a node whose host has never booted. Skip by
# default; --strict turns those into failures.
host_data_missing() {
  if ((STRICT)); then fail "$1" "$2"; else skip "$1" "$2"; fi
}

# ---------------------------------------------------------------------------
section "1. transport and authentication"

rf_get /redfish/v1 || true
status_check "service root reachable over HTTPS" 200
body_check "RedfishVersion is 1.13.0 (bmclib floor)" '.RedfishVersion == "1.13.0"'
body_check "service root links Systems/Managers/Chassis/SessionService/UpdateService/Registries" \
  '.Systems and .Managers and .Chassis and .SessionService and .UpdateService and .Registries'
body_check "service root has Links.Sessions (gofish login path)" \
  '.Links.Sessions."@odata.id" == "/redfish/v1/SessionService/Sessions"'
ROOT_UUID="$(body_get .UUID)"
body_check "service root has UUID" '.UUID | length > 0'

HTTP_CODE="$(curl -s -m "$RF_TIMEOUT" -o /dev/null -w '%{http_code}' "http://$HOST/redfish/v1" 2>/dev/null || echo 000)"
if [[ "$HTTP_CODE" =~ ^30[1278]$ ]]; then
  pass "plain HTTP redirects to HTTPS ($HTTP_CODE)"
else
  fail "plain HTTP redirects to HTTPS" "got HTTP $HTTP_CODE"
fi

NOAUTH="$(curl -sk -m "$RF_TIMEOUT" -o /dev/null -w '%{http_code}' "$RF_BASE/redfish/v1/Systems" 2>/dev/null || echo 000)"
[[ "$NOAUTH" == 401 ]] && pass "protected resource rejects missing credentials (401)" ||
  fail "protected resource rejects missing credentials" "got HTTP $NOAUTH"

# Deliberate ~2s server-side delay on failure + bcrypt: allow extra time.
BADAUTH="$(curl -sk -m 25 -u "$RF_USER:definitely-wrong-$RANDOM" -o /dev/null -w '%{http_code}' "$RF_BASE/redfish/v1/Systems" 2>/dev/null || echo 000)"
[[ "$BADAUTH" == 401 ]] && pass "protected resource rejects bad password (401)" ||
  fail "protected resource rejects bad password" "got HTTP $BADAUTH"

get_ok "basic auth accepted on protected resource" /redfish/v1/Systems

MDCODE="$(curl -sk -m "$RF_TIMEOUT" -o /dev/null -w '%{http_code}' "$RF_BASE/redfish/v1/\$metadata" 2>/dev/null || echo 000)"
[[ "$MDCODE" == 200 ]] && pass "\$metadata served without auth" ||
  fail "\$metadata served without auth" "got HTTP $MDCODE"
OACODE="$(curl -sk -m "$RF_TIMEOUT" -o /dev/null -w '%{http_code}' "$RF_BASE/redfish/v1/openapi.json" 2>/dev/null || echo 000)"
[[ "$OACODE" == 200 ]] && pass "openapi.json served" || fail "openapi.json served" "got HTTP $OACODE"

# ---------------------------------------------------------------------------
section "2. collections"

get_ok "GET /redfish/v1/Systems" /redfish/v1/Systems &&
  body_check "Systems has exactly member /redfish/v1/Systems/1" \
    '."Members@odata.count" == 1 and .Members[0]."@odata.id" == "/redfish/v1/Systems/1"'
get_ok "GET /redfish/v1/Managers" /redfish/v1/Managers &&
  body_check "Managers has exactly member /redfish/v1/Managers/1" \
    '."Members@odata.count" == 1 and .Members[0]."@odata.id" == "/redfish/v1/Managers/1"'
get_ok "GET /redfish/v1/Chassis" /redfish/v1/Chassis &&
  body_check "Chassis has exactly member /redfish/v1/Chassis/1" \
    '."Members@odata.count" == 1 and .Members[0]."@odata.id" == "/redfish/v1/Chassis/1"'
get_ok "GET /redfish/v1/Registries" /redfish/v1/Registries

# Services this build deliberately does not implement must 404, and the crawl
# above must not have advertised them.
for absent in EventService AccountService TaskService; do
  rf_get "/redfish/v1/$absent"
  [[ "$RF_STATUS" == 404 ]] && pass "unimplemented $absent returns 404" ||
    fail "unimplemented $absent returns 404" "got HTTP $RF_STATUS"
done

# ---------------------------------------------------------------------------
section "3. computer system"

SYS_BODY="" BIOS_VERSION=""
if get_ok "GET /redfish/v1/Systems/1" /redfish/v1/Systems/1; then
  SYS_BODY="$RF_BODY"
  body_check "PowerState is On or Off" '.PowerState == "On" or .PowerState == "Off"'
  body_check "SystemType is Physical" '.SystemType == "Physical"'
  body_check "Reset action advertises exactly On/ForceOff/GracefulShutdown/ForceRestart/PowerCycle" \
    '.Actions."#ComputerSystem.Reset"."ResetType@Redfish.AllowableValues" as $v
     | ($v | sort) == (["On","ForceOff","GracefulShutdown","ForceRestart","PowerCycle"] | sort)'
  body_check "Boot override allowables complete (incl. Pxe/Usb/Cd/BiosSetup/UefiHttp)" \
    '(.Boot."BootSourceOverrideEnabled@Redfish.AllowableValues" as $e
      | ["Disabled","Once","Continuous"] - $e == [])
     and (.Boot."BootSourceOverrideTarget@Redfish.AllowableValues" as $t
      | ["None","Pxe","Hdd","Usb","Cd","BiosSetup","UefiHttp"] - $t == [])'
  body_check "Boot override mode is UEFI" '.Boot.BootSourceOverrideMode == "UEFI"'
  body_check "links to Chassis/1 and Managers/1" \
    '(.Links.Chassis[0]."@odata.id" == "/redfish/v1/Chassis/1")
     and (.Links.ManagedBy[0]."@odata.id" == "/redfish/v1/Managers/1")'
  body_check "sub-resource links present (Bios/SecureBoot/Memory/Processors/Storage/EthernetInterfaces)" \
    '.Bios and .SecureBoot and .Memory and .Processors and .Storage and .EthernetInterfaces'
  if jq -e '(.Manufacturer|length>0) and (.Model|length>0) and (.SerialNumber|length>0) and (.UUID|length>0) and (.BiosVersion|length>0)' >/dev/null 2>&1 <<<"$SYS_BODY"; then
    BIOS_VERSION="$(body_get .BiosVersion)"
    pass "host-reported identity populated (Manufacturer/Model/SerialNumber/UUID/BiosVersion)"
  else
    host_data_missing "host-reported identity populated" "host has not pushed its inventory yet"
  fi
fi

if get_ok "GET /redfish/v1/Systems/1/Bios" /redfish/v1/Systems/1/Bios; then
  N_ATTRS="$(jq -r '.Attributes | length' <<<"$RF_BODY" 2>/dev/null || echo 0)"
  BIOS_ATTRS="$(jq -c '.Attributes // {}' <<<"$RF_BODY" 2>/dev/null || echo '{}')"
  if ((N_ATTRS > 0)); then
    pass "BIOS attributes populated ($N_ATTRS attributes)"
    body_check "BIOS settings object advertised, apply OnReset" \
      '."@Redfish.Settings".SettingsObject."@odata.id" == "/redfish/v1/Systems/1/Bios/Settings"
       and (."@Redfish.Settings".SupportedApplyTimes | index("OnReset"))'
    body_check "AttributeRegistry named" '.AttributeRegistry | length > 0'
  else
    host_data_missing "BIOS attributes populated" "host has not pushed BIOS attributes yet"
  fi
else
  BIOS_ATTRS='{}'
fi
BIOS_STAGED=""
if get_ok "GET /redfish/v1/Systems/1/Bios/Settings" /redfish/v1/Systems/1/Bios/Settings; then
  BIOS_STAGED="$(jq -c '.Attributes // {}' <<<"$RF_BODY" 2>/dev/null || echo '{}')"
fi
get_ok "GET /redfish/v1/Systems/1/BootOptions" /redfish/v1/Systems/1/BootOptions
get_ok "GET /redfish/v1/Systems/1/SecureBoot" /redfish/v1/Systems/1/SecureBoot
get_ok "GET /redfish/v1/Systems/1/Memory" /redfish/v1/Systems/1/Memory
get_ok "GET /redfish/v1/Systems/1/Processors" /redfish/v1/Systems/1/Processors
if get_ok "GET /redfish/v1/Systems/1/Storage" /redfish/v1/Systems/1/Storage; then
  body_check "storage subsystems 1 (host) and BMC (gadget) present" \
    '(.Members | map(."@odata.id")) as $m
     | ($m | index("/redfish/v1/Systems/1/Storage/1")) and ($m | index("/redfish/v1/Systems/1/Storage/BMC"))'
fi

# ---------------------------------------------------------------------------
section "4. manager (BMC)"

NODE_VERSION=""
if get_ok "GET /redfish/v1/Managers/1" /redfish/v1/Managers/1; then
  body_check "ManagerType BMC, Sipeed NanoKVM, status Enabled/OK" \
    '.ManagerType == "BMC" and .Manufacturer == "Sipeed" and .Model == "NanoKVM"
     and .Status.State == "Enabled" and .Status.Health == "OK"'
  MGR_UUID="$(body_get .UUID)"
  [[ -n "$MGR_UUID" && "$MGR_UUID" == "$ROOT_UUID" ]] &&
    pass "manager UUID matches service root UUID" ||
    fail "manager UUID matches service root UUID" "manager=$MGR_UUID root=$ROOT_UUID"
  NODE_VERSION="$(body_get .FirmwareVersion)"
  if [[ -n "$NODE_VERSION" && "$NODE_VERSION" != "1.0.0" ]]; then
    pass "FirmwareVersion is a real build version ($NODE_VERSION)"
  else
    fail "FirmwareVersion is a real build version" "got '${NODE_VERSION:-<empty>}' (unstamped build falls back to 1.0.0)"
  fi
  body_check "manager links back to Systems/1" \
    '.Links.ManagerForServers[0]."@odata.id" == "/redfish/v1/Systems/1"'
  body_check "Dell OEM shim present (empty Oem.Dell for gofish)" '.Oem.Dell != null'
fi

if get_ok "GET manager EthernetInterfaces" /redfish/v1/Managers/1/EthernetInterfaces; then
  body_check "eth0 interface present" \
    '.Members | map(."@odata.id") | index("/redfish/v1/Managers/1/EthernetInterfaces/eth0")'
fi
if get_ok "GET manager eth0 interface" /redfish/v1/Managers/1/EthernetInterfaces/eth0; then
  body_check "eth0 has MAC, IPv4 address, link up" \
    '(.MACAddress | test("^([0-9a-f]{2}:){5}[0-9a-f]{2}$"; "i"))
     and (.IPv4Addresses[0].Address | length > 0)
     and .LinkStatus == "LinkUp" and .InterfaceEnabled == true'
fi
get_ok "GET manager NetworkInterfaces (empty collection ok)" /redfish/v1/Managers/1/NetworkInterfaces

SERIAL_BODY=""
if get_ok "GET manager SerialInterfaces/1" /redfish/v1/Managers/1/SerialInterfaces/1; then
  SERIAL_BODY="$RF_BODY"
  body_check "serial console is 115200 8N1 (string-typed fields)" \
    '.BitRate == "115200" and .DataBits == "8" and .Parity == "None" and .StopBits == "1"'
fi

if get_ok "GET VirtualMedia collection" /redfish/v1/Managers/1/VirtualMedia; then
  body_check "CD virtual media present" \
    '.Members | map(."@odata.id") | index("/redfish/v1/Managers/1/VirtualMedia/CD")'
fi
if get_ok "GET VirtualMedia/CD" /redfish/v1/Managers/1/VirtualMedia/CD; then
  body_check "CD advertises InsertMedia and EjectMedia actions" \
    '.Actions."#VirtualMedia.InsertMedia".target and .Actions."#VirtualMedia.EjectMedia".target'
  body_check "CD media state coherent (ConnectedVia URI iff inserted)" \
    'if .Inserted then .ConnectedVia == "URI" else .ConnectedVia == "NotConnected" end'
fi

# ---------------------------------------------------------------------------
section "5. chassis, thermal, sensors"

if get_ok "GET /redfish/v1/Chassis/1" /redfish/v1/Chassis/1; then
  body_check "chassis Module, status Enabled/OK, links coherent" \
    '.ChassisType == "Module" and .Status.State == "Enabled" and .Status.Health == "OK"
     and .Links.ComputerSystems[0]."@odata.id" == "/redfish/v1/Systems/1"'
fi

SENSOR_FRESH=0
if get_ok "GET Chassis/1/Sensors" /redfish/v1/Chassis/1/Sensors; then
  if jq -e '.Members | map(."@odata.id") | index("/redfish/v1/Chassis/1/Sensors/SoCTemp")' >/dev/null 2>&1 <<<"$RF_BODY"; then
    if get_ok "GET Sensors/SoCTemp" /redfish/v1/Chassis/1/Sensors/SoCTemp; then
      if jq -e '.Oem.NanoKVM.Stale == false' >/dev/null 2>&1 <<<"$RF_BODY"; then
        SENSOR_FRESH=1
        body_check "host sensor push healthy (fresh, valid, last push OK, Enabled/OK)" \
          '.Oem.NanoKVM.TemperatureValid == true and .Oem.NanoKVM.LastPushOK == true
           and .Status.State == "Enabled" and (.Reading | type == "number")'
      else
        host_data_missing "host sensor push healthy" "sensor stale (host off or bmc-sensord not running)"
      fi
    fi
  else
    host_data_missing "SoCTemp sensor present" "EEPROM sensor reader unavailable on this node"
  fi
fi

if get_ok "GET Chassis/1/Thermal" /redfish/v1/Chassis/1/Thermal; then
  if ((SENSOR_FRESH)); then
    body_check "SoC temperature reading plausible (0..110 C)" \
      '[.Temperatures[]? | select(.MemberId == "SoC")][0].ReadingCelsius as $t
       | $t != null and $t > 0 and $t < 110'
  else
    skip "SoC temperature reading plausible" "sensor not fresh; Thermal omits stale readings"
  fi
  # Fans is emitted only when the I2C sensor block has a valid fan reading
  # (api/redfish/chassis.go thermalBody); fan-less platforms omit it entirely.
  if jq -e '.Fans' >/dev/null 2>&1 <<<"$RF_BODY"; then
    body_check "ActiveCooler fan reported (percent + PiBmc level)" \
      '[.Fans[]? | select(.MemberId == "ActiveCooler")][0] as $f
       | $f != null and $f.ReadingUnits == "Percent" and ($f.Oem.PiBmc.MaxLevel | type == "number")'
  else
    skip "ActiveCooler fan reported" "no fan telemetry on this platform"
  fi
fi

# ---------------------------------------------------------------------------
section "6. update service"

if get_ok "GET /redfish/v1/UpdateService" /redfish/v1/UpdateService; then
  body_check "enabled, capsule push URI advertised" \
    '.ServiceEnabled == true and .HttpPushUri == "/redfish/v1/UpdateService/update"'
  body_check "SimpleUpdate action advertised (HTTP/HTTPS)" \
    '.Actions."#UpdateService.SimpleUpdate" as $a
     | $a.target and (["HTTP","HTTPS"] - $a."TransferProtocol@Redfish.AllowableValues" == [])'
fi
if get_ok "GET FirmwareInventory" /redfish/v1/UpdateService/FirmwareInventory; then
  FW_MEMBER="$(jq -r '.Members[0]."@odata.id" // empty' <<<"$RF_BODY" 2>/dev/null)"
  if [[ -n "$FW_MEMBER" ]]; then
    if get_ok "GET ${FW_MEMBER#/redfish/v1/}" "$FW_MEMBER"; then
      body_check "firmware entry updateable with version + ESRT GUID" \
        '.Updateable == true and (.Version | length > 0) and (.SoftwareId | length > 0)'
      if [[ -n "$BIOS_VERSION" ]]; then
        FW_VERSION="$(body_get .Version)"
        [[ "$FW_VERSION" == "$BIOS_VERSION" ]] &&
          pass "firmware inventory version matches system BiosVersion ($FW_VERSION)" ||
          fail "firmware inventory version matches system BiosVersion" "inventory=$FW_VERSION system=$BIOS_VERSION"
      fi
    fi
  else
    host_data_missing "firmware inventory entry present" "host has not pushed ESRT info yet"
  fi
else
  FW_MEMBER=""
fi

# ---------------------------------------------------------------------------
section "7. session lifecycle"

# Session create pays full bcrypt; cold nodes need generous timeouts.
RESP="$(curl -sk -m 30 -D - -X POST -H 'Content-Type: application/json' \
  -d "{\"UserName\":\"$RF_USER\",\"Password\":\"$RF_PASS\"}" \
  "$RF_BASE/redfish/v1/SessionService/Sessions" 2>/dev/null)" || RESP=""
SESSION_CODE="$(sed -n 's|^HTTP/[0-9.]* \([0-9]*\).*|\1|p' <<<"$RESP" | head -1)"
TOKEN="$(sed -n 's/^[Xx]-[Aa]uth-[Tt]oken: *//p' <<<"$RESP" | tr -d '\r' | head -1)"
SESSION_LOC="$(sed -n 's/^[Ll]ocation: *//p' <<<"$RESP" | tr -d '\r' | head -1)"

if [[ "$SESSION_CODE" == 201 && -n "$TOKEN" && -n "$SESSION_LOC" ]]; then
  pass "session created (201 + X-Auth-Token + Location)"
  TOKCODE="$(curl -sk -m "$RF_TIMEOUT" -H "X-Auth-Token: $TOKEN" -o /dev/null -w '%{http_code}' "$RF_BASE/redfish/v1/Systems/1" 2>/dev/null || echo 000)"
  [[ "$TOKCODE" == 200 ]] && pass "session token accepted on protected resource" ||
    fail "session token accepted on protected resource" "got HTTP $TOKCODE"
  DELCODE="$(curl -sk -m "$RF_TIMEOUT" -H "X-Auth-Token: $TOKEN" -X DELETE -o /dev/null -w '%{http_code}' "$RF_BASE$SESSION_LOC" 2>/dev/null || echo 000)"
  # DELETE is a deliberate no-op stub (stateless JWTs) but must answer 204.
  [[ "$DELCODE" == 204 ]] && pass "session delete answers 204" ||
    fail "session delete answers 204" "DELETE $SESSION_LOC -> HTTP $DELCODE"
else
  fail "session created (201 + X-Auth-Token + Location)" \
    "code=${SESSION_CODE:-none} token=${TOKEN:+present}${TOKEN:-missing} location=${SESSION_LOC:-missing}"
fi

# ---------------------------------------------------------------------------
section "8. write protocol (safe negatives)"

# Invalid ResetType must 400 without touching power.
rf_post /redfish/v1/Systems/1/Actions/ComputerSystem.Reset '{"ResetType":"Nmi"}'
[[ "$RF_STATUS" == 400 ]] && pass "invalid ResetType rejected with 400" ||
  fail "invalid ResetType rejected with 400" "got HTTP $RF_STATUS"

# Host-reported identity is writable only over the host interface.
rf_patch /redfish/v1/Systems/1 '{"Manufacturer":"conformance-probe"}'
[[ "$RF_STATUS" == 403 ]] && pass "host-lane fields rejected from LAN (403)" ||
  fail "host-lane fields rejected from LAN (403)" "PATCH Manufacturer -> HTTP $RF_STATUS"

if [[ -n "${FW_MEMBER:-}" ]]; then
  rf_patch "$FW_MEMBER" '{"Version":"conformance-probe"}'
  [[ "$RF_STATUS" == 403 ]] && pass "firmware inventory PATCH rejected from LAN (403)" ||
    fail "firmware inventory PATCH rejected from LAN (403)" "got HTTP $RF_STATUS"
else
  skip "firmware inventory PATCH rejected from LAN" "no inventory member to probe"
fi

# Redfish error bodies carry the standard envelope.
rf_get /redfish/v1/Systems/1/Memory/definitely-absent
[[ "$RF_STATUS" == 404 ]] &&
  jq -e '.error.code and .error."@Message.ExtendedInfo"' >/dev/null 2>&1 <<<"$RF_BODY" &&
  pass "404 error uses Redfish error envelope" ||
  fail "404 error uses Redfish error envelope" "HTTP $RF_STATUS body: $(head -c 120 <<<"$RF_BODY")"

# ---------------------------------------------------------------------------
section "9. modifications (round-trip, restores prior state)"

if ((!MUTATE)); then
  skip "boot override round-trip" "--read-only"
  skip "serial interface PATCH" "--read-only"
  skip "BIOS settings staging round-trip" "--read-only"
  skip "virtual media insert/eject" "--read-only"
elif [[ -z "$SYS_BODY" ]]; then
  fail "boot override round-trip" "Systems/1 unreadable, cannot mutate safely"
else
  # 9a. Boot override: stage Once/Pxe, verify, restore what was there.
  ORIG_EN="$(jq -r '.Boot.BootSourceOverrideEnabled' <<<"$SYS_BODY")"
  ORIG_TGT="$(jq -r '.Boot.BootSourceOverrideTarget' <<<"$SYS_BODY")"
  rf_patch /redfish/v1/Systems/1 '{"Boot":{"BootSourceOverrideEnabled":"Once","BootSourceOverrideTarget":"Pxe"}}'
  if [[ "$RF_STATUS" == 200 ]]; then
    rf_get /redfish/v1/Systems/1
    jq -e '.Boot.BootSourceOverrideEnabled == "Once" and .Boot.BootSourceOverrideTarget == "Pxe"' >/dev/null 2>&1 <<<"$RF_BODY" &&
      pass "boot override PATCH applied and visible (Once/Pxe)" ||
      fail "boot override PATCH applied and visible" "readback: $(jq -c .Boot <<<"$RF_BODY" 2>/dev/null | head -c 200)"
    rf_patch /redfish/v1/Systems/1 \
      "{\"Boot\":{\"BootSourceOverrideEnabled\":\"$ORIG_EN\",\"BootSourceOverrideTarget\":\"$ORIG_TGT\"}}"
    if [[ "$RF_STATUS" == 200 ]]; then
      rf_get /redfish/v1/Systems/1
      jq -e --arg e "$ORIG_EN" --arg t "$ORIG_TGT" \
        '.Boot.BootSourceOverrideEnabled == $e and .Boot.BootSourceOverrideTarget == $t' >/dev/null 2>&1 <<<"$RF_BODY" &&
        pass "boot override restored ($ORIG_EN/$ORIG_TGT)" ||
        fail "boot override restored" "readback: $(jq -c .Boot <<<"$RF_BODY" 2>/dev/null | head -c 200)"
    else
      fail "boot override restored" "restore PATCH -> HTTP $RF_STATUS (manual cleanup needed: $ORIG_EN/$ORIG_TGT)"
    fi
  else
    fail "boot override PATCH accepted" "PATCH Systems/1 -> HTTP $RF_STATUS: $(head -c 200 <<<"$RF_BODY")"
  fi

  # 9b. Serial interface: PATCH current values back (no-op write must 200).
  if [[ -n "$SERIAL_BODY" ]]; then
    CUR_RATE="$(jq -r '.BitRate // empty' <<<"$SERIAL_BODY")"
    if [[ -n "$CUR_RATE" ]]; then
      rf_patch /redfish/v1/Managers/1/SerialInterfaces/1 "{\"BitRate\":\"$CUR_RATE\"}"
      [[ "$RF_STATUS" == 200 ]] && pass "serial interface PATCH accepted (no-op $CUR_RATE)" ||
        fail "serial interface PATCH accepted" "got HTTP $RF_STATUS"
    else
      skip "serial interface PATCH" "no BitRate configured to round-trip"
    fi
  else
    skip "serial interface PATCH" "serial interface unreadable"
  fi

  # 9c. BIOS settings staging: stage one attribute at its current value
  # (a no-op even if the host consumes it), verify, restore the staged set.
  ATTR_NAME="$(jq -r 'if has("FanMode") then "FanMode" else (keys_unsorted[0] // empty) end' <<<"$BIOS_ATTRS" 2>/dev/null)"
  if [[ -z "$BIOS_STAGED" ]]; then
    skip "BIOS settings staging round-trip" "Bios/Settings unreadable, cannot restore safely"
  elif [[ -n "$ATTR_NAME" ]]; then
    ATTR_VAL="$(jq -c --arg k "$ATTR_NAME" '.[$k]' <<<"$BIOS_ATTRS")"
    rf_patch /redfish/v1/Systems/1/Bios/Settings "{\"Attributes\":{\"$ATTR_NAME\":$ATTR_VAL}}"
    if [[ "$RF_STATUS" == 200 ]]; then
      rf_get /redfish/v1/Systems/1/Bios/Settings
      jq -e --arg k "$ATTR_NAME" --argjson v "$ATTR_VAL" '.Attributes[$k] == $v' >/dev/null 2>&1 <<<"$RF_BODY" &&
        pass "BIOS settings staged and visible ($ATTR_NAME)" ||
        fail "BIOS settings staged and visible" "staged $ATTR_NAME not in readback"
      rf_patch /redfish/v1/Systems/1/Bios/Settings "{\"Attributes\":$BIOS_STAGED}"
      [[ "$RF_STATUS" == 200 ]] && pass "BIOS staged set restored" ||
        fail "BIOS staged set restored" "restore PATCH -> HTTP $RF_STATUS (manual cleanup: Attributes=$BIOS_STAGED)"
    else
      fail "BIOS settings staging accepted" "PATCH Bios/Settings -> HTTP $RF_STATUS: $(head -c 200 <<<"$RF_BODY")"
    fi
  else
    host_data_missing "BIOS settings staging round-trip" "no host-reported attribute to stage safely"
  fi

  # 9d. Virtual media (only with an operator-supplied image URL).
  if [[ -n "$ISO_URL" ]]; then
    rf_get /redfish/v1/Managers/1/VirtualMedia/CD
    if jq -e '.Inserted == true' >/dev/null 2>&1 <<<"$RF_BODY"; then
      skip "virtual media insert/eject" "media already inserted; not touching it"
    else
      rf_req POST /redfish/v1/Managers/1/VirtualMedia/CD/Actions/VirtualMedia.InsertMedia \
        -H 'Content-Type: application/json' -d "{\"Image\":\"$ISO_URL\"}" -m 120
      if [[ "$RF_STATUS" == 200 ]]; then
        rf_get /redfish/v1/Managers/1/VirtualMedia/CD
        jq -e '.Inserted == true and .ConnectedVia == "URI"' >/dev/null 2>&1 <<<"$RF_BODY" &&
          pass "virtual media inserted from URL" ||
          fail "virtual media inserted from URL" "not Inserted/URI after InsertMedia"
        rf_post /redfish/v1/Managers/1/VirtualMedia/CD/Actions/VirtualMedia.EjectMedia ''
        [[ "$RF_STATUS" == 204 ]] && pass "virtual media ejected (204)" ||
          fail "virtual media ejected" "EjectMedia -> HTTP $RF_STATUS (manual eject needed!)"
      else
        fail "virtual media inserted from URL" "InsertMedia -> HTTP $RF_STATUS: $(head -c 200 <<<"$RF_BODY")"
      fi
    fi
  else
    skip "virtual media insert/eject" "no --iso-url given"
  fi
fi

# ---------------------------------------------------------------------------
echo
[[ -n "$NODE_VERSION" ]] && echo "NODE_VERSION=$NODE_VERSION"
summary "conformance $HOST"
