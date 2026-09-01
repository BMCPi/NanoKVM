# shellcheck shell=bash
# Shared helpers for the NanoKVM Redfish conformance suite.
# Sourced by redfish-conformance.sh and fleet-check.sh — not executable on its own.

RF_USER="${RF_USER:-admin}"
RF_PASS="${RF_PASS:-admin}"
RF_TIMEOUT="${RF_TIMEOUT:-15}"

_PASS=0
_FAIL=0
_SKIP=0
_FAILED_NAMES=()
_SECTION=""

if [[ -t 1 ]]; then
  _C_OK=$'\033[32m' _C_BAD=$'\033[31m' _C_DIM=$'\033[2m' _C_OFF=$'\033[0m'
else
  _C_OK="" _C_BAD="" _C_DIM="" _C_OFF=""
fi

section() {
  _SECTION="$1"
  printf '\n%s## %s%s\n' "$_C_DIM" "$1" "$_C_OFF"
}

pass() { _PASS=$((_PASS + 1)); printf '  %sok%s   %s\n' "$_C_OK" "$_C_OFF" "$1"; }
skip() { _SKIP=$((_SKIP + 1)); printf '  skip %s (%s)\n' "$1" "$2"; }
fail() {
  _FAIL=$((_FAIL + 1))
  _FAILED_NAMES+=("[$_SECTION] $1")
  printf '  %sFAIL%s %s\n' "$_C_BAD" "$_C_OFF" "$1"
  [[ -n "${2:-}" ]] && printf '       %s%s%s\n' "$_C_DIM" "$2" "$_C_OFF"
}

# check NAME COMMAND... — pass/fail on the command's exit status.
check() {
  local name="$1"
  shift
  if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name" "command failed: $*"; fi
}

summary() {
  printf '\n%s: %d passed, %d failed, %d skipped\n' "${1:-conformance}" "$_PASS" "$_FAIL" "$_SKIP"
  if ((_FAIL > 0)); then
    printf 'failed checks:\n'
    printf '  - %s\n' "${_FAILED_NAMES[@]}"
  fi
  # Machine-readable trailer for fleet-check.sh.
  printf 'RESULT pass=%d fail=%d skip=%d\n' "$_PASS" "$_FAIL" "$_SKIP"
  ((_FAIL == 0))
}

# --- HTTP helpers -----------------------------------------------------------
# All requests capture status into RF_STATUS and body into RF_BODY.

RF_STATUS=""
RF_BODY=""

# rf_req METHOD PATH [curl-args...] — authenticated request against $RF_BASE.
rf_req() {
  local method="$1" path="$2"
  shift 2
  RF_BODY="$(curl -sk -m "$RF_TIMEOUT" -u "$RF_USER:$RF_PASS" -X "$method" \
    -w '\n%{http_code}' "$@" "$RF_BASE$path" 2>/dev/null)" || {
    RF_STATUS="000"
    RF_BODY=""
    return 1
  }
  RF_STATUS="${RF_BODY##*$'\n'}"
  RF_BODY="${RF_BODY%$'\n'*}"
}

rf_get() { rf_req GET "$1"; }
rf_patch() { rf_req PATCH "$1" -H 'Content-Type: application/json' -d "$2"; }
rf_post() { rf_req POST "$1" -H 'Content-Type: application/json' -d "$2"; }
rf_delete() { rf_req DELETE "$1"; }

# get_ok NAME PATH — GET must return 200 with valid JSON; body left in RF_BODY.
get_ok() {
  local name="$1" path="$2"
  rf_get "$path"
  if [[ "$RF_STATUS" != "200" ]]; then
    fail "$name" "GET $path -> HTTP $RF_STATUS"
    return 1
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"$RF_BODY"; then
    fail "$name" "GET $path -> invalid JSON"
    return 1
  fi
  pass "$name"
}

# body_check NAME JQ_EXPR — assert a jq boolean expression over the last RF_BODY.
body_check() {
  local name="$1" expr="$2"
  if jq -e "$expr" >/dev/null 2>&1 <<<"$RF_BODY"; then
    pass "$name"
  else
    fail "$name" "jq: $expr"
  fi
}

# body_get JQ_EXPR — print a raw value from the last RF_BODY (empty on miss).
body_get() { jq -r "$1 // empty" 2>/dev/null <<<"$RF_BODY"; }

# status_check NAME EXPECTED_RE — assert the last RF_STATUS matches a regex.
status_check() {
  local name="$1" want="$2"
  if [[ "$RF_STATUS" =~ ^($want)$ ]]; then
    pass "$name"
  else
    fail "$name" "HTTP $RF_STATUS (want $want)"
  fi
}

# node_version_hash — 12-char commit hash embedded in Manager.FirmwareVersion
# (Go pseudo-version vX.Y.Z-0.YYYYMMDDHHMMSS-abcdef123456), or empty.
node_version_hash() {
  rf_get /redfish/v1/Managers/1 || return 1
  body_get .FirmwareVersion | grep -oE '[0-9a-f]{12}$'
}
