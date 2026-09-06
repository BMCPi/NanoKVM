#!/usr/bin/env bash
# fleet-check.sh — run the standard Redfish conformance list across the fleet
# and converge non-conforming nodes:
#
#   conformance FAIL + stale build  -> deploy current build (make deploy), retest
#   conformance FAIL + current build -> deep-diagnose (full API dump + report)
#   conformance PASS                 -> done
#
# Usage: fleet-check.sh [options] [host...]
#   --hosts FILE     newline-separated hosts (# comments ok); default: args
#   --discover       find hosts via avahi-browse -rt _redfish._tcp
#   -u USER:PASS     credentials for every node (default admin:admin)
#   --no-update      never deploy; just classify and diagnose
#   --converge       also deploy to stale nodes that pass conformance
#   --strict         pass --strict to the conformance suite
#   --report DIR     where reports go (default tools/conformance/reports/<ts>)
#
# "Up to date" means the node's /api/application/version current equals this
# checkout's `git describe --tags --always` (what make deploy stamps), the
# same equality rule pkg/app/autoupdate applies.
set -uo pipefail

TOOL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TOOL_DIR/../.." && pwd)"

die() { echo "fleet-check: error: $*" >&2; exit 2; }
usage() { sed -n '2,18p' "${BASH_SOURCE[0]}"; exit 2; }

RF_USER="${RF_USER:-admin}"
RF_PASS="${RF_PASS:-admin}"
HOSTS=() HOSTS_FILE="" DISCOVER=0 DO_UPDATE=1 CONVERGE=0 STRICT_FLAG=()
REPORT_DIR=""
while (($#)); do
  case "$1" in
  --hosts) HOSTS_FILE="$2"; shift ;;
  --discover) DISCOVER=1 ;;
  -u) [[ "$2" == *:* ]] || die "-u wants USER:PASS"; RF_USER="${2%%:*}" RF_PASS="${2#*:}"; shift ;;
  --no-update) DO_UPDATE=0 ;;
  --converge) CONVERGE=1 ;;
  --strict) STRICT_FLAG=(--strict) ;;
  --report) REPORT_DIR="$2"; shift ;;
  -h | --help) usage ;;
  -*) die "unknown flag $1" ;;
  *) HOSTS+=("$1") ;;
  esac
  shift
done
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"

if [[ -n "$HOSTS_FILE" ]]; then
  [[ -r "$HOSTS_FILE" ]] || die "cannot read $HOSTS_FILE"
  while IFS= read -r line; do
    line="${line%%#*}"
    line="$(tr -d '[:space:]' <<<"$line")"
    [[ -n "$line" ]] && HOSTS+=("$line")
  done <"$HOSTS_FILE"
fi
if ((DISCOVER)); then
  command -v avahi-browse >/dev/null || die "--discover needs avahi-browse"
  while IFS= read -r addr; do
    [[ -n "$addr" ]] && HOSTS+=("$addr")
  done < <(avahi-browse -rtp _redfish._tcp 2>/dev/null |
    awk -F';' '$1 == "=" && $8 ~ /^[0-9.]+$/ { print $8 }' | sort -u)
fi
((${#HOSTS[@]})) || die "no hosts (args, --hosts FILE, or --discover)"

EXPECTED_VERSION="$(git -C "$REPO_ROOT" describe --tags --always | sed 's/^v//')"
[[ -n "$EXPECTED_VERSION" ]] || die "cannot compute expected version from git"

REPORT_DIR="${REPORT_DIR:-$TOOL_DIR/reports/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$REPORT_DIR"

export RF_USER RF_PASS

# --- app-API auth (mirrors the Makefile deploy recipe) ----------------------
KVM_SECRET="$(sed -n 's/^const SecretKey = "\(.*\)"$/\1/p' "$REPO_ROOT/pkg/app/auth/encrypt.go")"

app_token() { # app_token HOST -> token on stdout, or nothing
  local host="$1" pw
  [[ -n "$KVM_SECRET" ]] || return 1
  pw="$(printf '%s' "$RF_PASS" |
    openssl enc -aes-256-cbc -md md5 -pass "pass:$KVM_SECRET" -base64 -A 2>/dev/null |
    sed -e 's/+/%2B/g' -e 's|/|%2F|g' -e 's/=/%3D/g')" || return 1
  curl -ksS -m 30 -X POST "https://$host/api/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$RF_USER\",\"password\":\"$pw\"}" 2>/dev/null |
    jq -r '.data.token // .token // empty' 2>/dev/null
}

node_current_version() { # node_current_version HOST TOKEN
  curl -ksS -m 20 "https://$1/api/application/version" \
    -H "Cookie: nano-kvm-token=$2" 2>/dev/null |
    jq -r '.data.current // .current // empty' 2>/dev/null
}

# --- deep diagnosis for a node that is current but non-conforming -----------
diagnose() { # diagnose HOST OUTDIR TOKEN
  local host="$1" out="$2" token="$3" path safe
  echo "  deep-diagnosing $host -> $out"
  mkdir -p "$out/redfish"
  local paths=(
    "" Systems Systems/1 Systems/1/Bios Systems/1/Bios/Settings Systems/1/BootOptions
    Systems/1/SecureBoot Systems/1/Memory Systems/1/Processors Systems/1/Storage
    Systems/1/Storage/1 Systems/1/Storage/BMC Systems/1/EthernetInterfaces
    Managers Managers/1 Managers/1/EthernetInterfaces Managers/1/EthernetInterfaces/eth0
    Managers/1/NetworkInterfaces Managers/1/SerialInterfaces Managers/1/SerialInterfaces/1
    Managers/1/VirtualMedia Managers/1/VirtualMedia/CD
    Chassis Chassis/1 Chassis/1/Thermal Chassis/1/Sensors Chassis/1/Sensors/SoCTemp
    UpdateService UpdateService/FirmwareInventory SessionService Registries
  )
  for path in "${paths[@]}"; do
    safe="${path//\//_}"
    curl -sk -m 20 -u "$RF_USER:$RF_PASS" "https://$host/redfish/v1/$path" \
      >"$out/redfish/${safe:-root}.json" 2>/dev/null
  done
  if [[ -n "$token" ]]; then
    curl -ksS -m 20 -H "Cookie: nano-kvm-token=$token" "https://$host/api/vm/info" \
      >"$out/api-vm-info.json" 2>/dev/null
    curl -ksS -m 20 -H "Cookie: nano-kvm-token=$token" "https://$host/api/application/version" \
      >"$out/api-version.json" 2>/dev/null
  fi
  {
    echo "# Diagnosis: $host ($(date -Is))"
    echo
    echo "Expected build: $EXPECTED_VERSION (this checkout: $(git -C "$REPO_ROOT" rev-parse --short HEAD))"
    echo "Node reports:   ${NODE_APP_VERSION:-unknown} (app API), $(jq -r '.FirmwareVersion // "?"' "$out/redfish/Managers_1.json" 2>/dev/null) (Redfish manager)"
    echo
    echo "## Failed checks"
    echo
    sed -n '/^failed checks:/,/^RESULT /p' "$out/../${host}.retest.log" 2>/dev/null ||
      sed -n '/^failed checks:/,/^RESULT /p' "$out/../${host}.log" 2>/dev/null
    echo
    echo "## Where to look"
    echo
    echo "- Full Redfish dump: redfish/*.json (compare against a passing node or a local run)"
    echo "- App identity: api-vm-info.json, api-version.json"
    echo "- Handlers live in api/redfish/ (route table: api/redfish/redfish.go)"
    echo "- Host-pushed data (BIOS attrs, firmware inventory, sensors) comes from the"
    echo "  managed host: skips there usually mean the host is off or its services"
    echo "  (bmc-sensord, UEFI redfish client) are not reporting — not a BMC defect."
  } >"$out/DIAGNOSIS.md"
}

# --- per-host flow ----------------------------------------------------------
declare -A STATUS
overall=0

run_suite() { # run_suite HOST LOGFILE
  "$TOOL_DIR/redfish-conformance.sh" -u "$RF_USER:$RF_PASS" "${STRICT_FLAG[@]}" "$1" 2>&1 | tee "$2"
  [[ "${PIPESTATUS[0]}" == 0 ]]
}

wait_redfish_ready() { # wait_redfish_ready HOST — poll until the service root answers
  local host="$1" i
  for ((i = 0; i < 24; i++)); do
    [[ "$(curl -sk -m 5 -o /dev/null -w '%{http_code}' "https://$host/redfish/v1" 2>/dev/null)" == 200 ]] && return 0
    sleep 5
  done
  return 1
}

deploy_and_retest() { # deploy_and_retest HOST — sets STATUS[$host]; returns 1 on any failure
  local host="$1"
  echo "  deploying $EXPECTED_VERSION"
  # The dist/ file targets have no source dependencies, so a leftover binary
  # from an older commit would be shipped as-is. Force a rebuild.
  rm -f "$REPO_ROOT/dist/server/NanoKVM-Server" "$REPO_ROOT/dist/rpiboot/rpiboot"
  if ! make -C "$REPO_ROOT" deploy KVM_HOST="$host" KVM_SCHEME=https \
    KVM_USER="$RF_USER" KVM_PASS="$RF_PASS" 2>&1 | tee "$REPORT_DIR/$host.deploy.log"; then
    STATUS[$host]="DEPLOY-FAILED (see $REPORT_DIR/$host.deploy.log)"
    return 1
  fi
  if ! wait_redfish_ready "$host"; then
    STATUS[$host]="DEPLOY-FAILED (Redfish did not come back within 120s)"
    return 1
  fi
  TOKEN="$(app_token "$host" || true)"
  NODE_APP_VERSION=""
  [[ -n "$TOKEN" ]] && NODE_APP_VERSION="$(node_current_version "$host" "$TOKEN")"
  if [[ "$NODE_APP_VERSION" != "$EXPECTED_VERSION" ]]; then
    STATUS[$host]="DEPLOY-FAILED (node reports ${NODE_APP_VERSION:-unknown}, expected $EXPECTED_VERSION)"
    return 1
  fi
  echo "  retesting after update"
  if run_suite "$host" "$REPORT_DIR/$host.retest.log"; then
    STATUS[$host]="PASS-AFTER-UPDATE"
    return 0
  fi
  echo "  still non-conforming on the expected build -> diagnosing"
  diagnose "$host" "$REPORT_DIR/$host.diagnosis" "$TOKEN"
  STATUS[$host]="FAIL-AFTER-UPDATE (diagnosis in $REPORT_DIR/$host.diagnosis)"
  return 1
}

for host in "${HOSTS[@]}"; do
  echo
  echo "=================================================================="
  echo "== $host"
  echo "=================================================================="

  if ! curl -sk -m 10 -o /dev/null "https://$host/redfish/v1" 2>/dev/null; then
    echo "  unreachable, skipping"
    STATUS[$host]="UNREACHABLE"
    overall=1
    continue
  fi

  TOKEN="$(app_token "$host" || true)"
  NODE_APP_VERSION=""
  [[ -n "$TOKEN" ]] && NODE_APP_VERSION="$(node_current_version "$host" "$TOKEN")"
  echo "  build: node=${NODE_APP_VERSION:-unknown} expected=$EXPECTED_VERSION"

  if run_suite "$host" "$REPORT_DIR/$host.log"; then
    if [[ "$NODE_APP_VERSION" == "$EXPECTED_VERSION" ]]; then
      STATUS[$host]="PASS (current)"
    elif ((CONVERGE && DO_UPDATE)); then
      echo "  conforming but stale (${NODE_APP_VERSION:-unknown}) -> converging"
      deploy_and_retest "$host" || overall=1
    else
      STATUS[$host]="PASS (conforming but on ${NODE_APP_VERSION:-unknown}, expected $EXPECTED_VERSION)"
    fi
    continue
  fi

  if [[ "$NODE_APP_VERSION" == "$EXPECTED_VERSION" ]]; then
    echo "  non-conforming but already on the expected build -> diagnosing"
    diagnose "$host" "$REPORT_DIR/$host.diagnosis" "$TOKEN"
    STATUS[$host]="FAIL-CURRENT (diagnosis in $REPORT_DIR/$host.diagnosis)"
    overall=1
    continue
  fi

  if ((!DO_UPDATE)); then
    STATUS[$host]="FAIL-STALE (node=${NODE_APP_VERSION:-unknown}, --no-update)"
    overall=1
    continue
  fi

  echo "  stale (${NODE_APP_VERSION:-unknown}) and non-conforming"
  deploy_and_retest "$host" || overall=1
done

echo
echo "=================================================================="
echo "== fleet summary (expected build: $EXPECTED_VERSION)"
echo "=================================================================="
for host in "${HOSTS[@]}"; do
  printf '  %-20s %s\n' "$host" "${STATUS[$host]:-?}"
done
echo
echo "reports: $REPORT_DIR"
exit "$overall"
