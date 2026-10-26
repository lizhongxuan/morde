#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "$VERIFY_DIR/../../.." && pwd)"
SERVER_DIR="$ROOT_DIR/server"

GATEWAY_URL="${CH_VERIFY_GATEWAY_URL:-}"
HOST_SUFFIX="${CH_VERIFY_HOST_SUFFIX:-}"
HOST_ID="${CH_VERIFY_HOST_ID:-}"
PRIVATE_PORT="${CH_VERIFY_PRIVATE_PREVIEW_PORT:-}"
HOST_COOKIE="${CH_VERIFY_HOST_COOKIE:-}"
export -n HOST_COOKIE
COOKIE_CONFIG=""
HTTP_BODY=""
HTTP_HEADERS=""
HTTP_CODE="000"
REQUEST_ID=0

# Never pass a supplied cookie to curl's environment or print it.
unset CH_VERIFY_HOST_COOKIE

skip() {
  printf 'SKIP %s %s\n' "$1" "$2"
}

valid_loopback_url() {
  local value="$1" port
  [[ "$value" =~ ^http://(127\.0\.0\.1|\[::1\])(:([0-9]{1,5}))?$ ]] || return 1
  port="${BASH_REMATCH[3]:-80}"
  ((10#$port > 0 && 10#$port <= 65535))
}

valid_host_suffix() {
  [[ "$1" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$ ]] &&
    ((${#1} <= 253))
}

valid_host_id() {
  [[ "$1" =~ ^h[a-z0-9]{7}$ ]]
}

valid_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] && ((10#$1 >= 1024 && 10#$1 <= 65535 && 10#$1 != 8080))
}

request_gateway() {
  local authority="$1" path="$2" config="${3:-}" result
  local -a args=(--silent --connect-timeout 2 --max-time 5 --max-redirs 0 --noproxy '*'
    --header "Host: $authority")
  [[ "$path" == /* && "$path" != *$'\r'* && "$path" != *$'\n'* ]] || return 2
  REQUEST_ID=$((REQUEST_ID + 1))
  HTTP_BODY="$VERIFY_TMPDIR/gateway-body-$REQUEST_ID"
  HTTP_HEADERS="$VERIFY_TMPDIR/gateway-headers-$REQUEST_ID"
  args+=(--output "$HTTP_BODY" --dump-header "$HTTP_HEADERS" --write-out '%{http_code}')
  [[ -z "$config" ]] || args+=(--config "$config")
  result="$(curl "${args[@]}" "${GATEWAY_URL%/}$path" 2>/dev/null || true)"
  if [[ "$result" =~ ^[0-9]{3}$ ]]; then
    HTTP_CODE="$result"
  else
    HTTP_CODE=000
  fi
}

gateway_unit_tests() {
  local pattern='^(TestTicketCookieControlRoutesAndFixedNotFound|TestCookieAuthHostAPIAndLogout|TestMissingAndWrongScopeCredentialsReturnSameNotFound|TestProxyStripsPlatformCredentialsAndInjectsAIOKey|TestProxyBodyLimit|TestPrivatePreviewTicketAndPortValidation|TestCredentialCookieTamperExpiryAndScope|TestWebSocketOriginAndTrackedConnection|TestWebSocketEchoTracksActivityAndClosesOnShutdown|TestSSEStreamsPastServerWriteTimeout|TestSlowUploadStreamsPastServerReadTimeout)$'
  command -v go >/dev/null 2>&1 || return 2
  (cd -- "$SERVER_DIR" && env -u CH_TEST_DATABASE_URL -u CH_OSB_URL -u CH_OSB_API_KEY \
    go test ./internal/gateway/... -run "$pattern")
}

check_gateway_listener() {
  local host_authority invalid_authority
  local expected_body='{"error":{"code":"not_found","message":"not found","details":{}}}'
  host_authority="$HOST_ID.$HOST_SUFFIX"
  printf '%s' "$expected_body" >"$VERIFY_TMPDIR/expected-404-body"

  request_gateway "$host_authority" /__ch/api/host
  [[ "$HTTP_CODE" == 404 ]] || return 1
  cmp -s -- "$HTTP_BODY" "$VERIFY_TMPDIR/expected-404-body" || return 1
  cp -- "$HTTP_BODY" "$VERIFY_TMPDIR/fixed-404-body"

  for invalid_authority in "bad..$HOST_SUFFIX" "h123456.$HOST_SUFFIX"; do
    request_gateway "$invalid_authority" /__ch/api/host
    [[ "$HTTP_CODE" == 404 ]] || return 1
    cmp -s -- "$HTTP_BODY" "$VERIFY_TMPDIR/fixed-404-body" || return 1
  done
}

check_denied_preview_ports() {
  local port
  for port in 80 8080; do
    request_gateway "$port-$HOST_ID.$HOST_SUFFIX" /
    [[ "$HTTP_CODE" == 404 ]] || return 1
  done
}

check_private_preview_redirect() {
  request_gateway "$PRIVATE_PORT-$HOST_ID.$HOST_SUFFIX" /
  [[ "$HTTP_CODE" == 302 ]] || return 1
  grep -Eiq "^Location: https?://$HOST_ID\\.$HOST_SUFFIX/__ch/preview-auth\\?" "$HTTP_HEADERS" &&
    grep -Eiq "^Location: .*port=$PRIVATE_PORT([&[:space:]]|$)" "$HTTP_HEADERS"
}

make_cookie_config() {
  if [[ ! "$HOST_COOKIE" =~ ^${HOST_ID}\.u[a-z0-9]{7}\.[0-9]{9,12}\.[a-f0-9]{64}$ ]]; then
    HOST_COOKIE=""
    return 1
  fi
  COOKIE_CONFIG="$VERIFY_TMPDIR/host-cookie.curl"
  printf 'header = "Cookie: ch_host=%s"\n' "$HOST_COOKIE" >"$COOKIE_CONFIG"
  chmod 600 "$COOKIE_CONFIG"
  HOST_COOKIE=""
}

check_cookie_authorization() {
  local other_host_id
  [[ -n "$COOKIE_CONFIG" ]] || return 2
  request_gateway "$HOST_ID.$HOST_SUFFIX" /__ch/api/host "$COOKIE_CONFIG"
  [[ "$HTTP_CODE" == 200 ]] || return 1
  [[ -s "$HTTP_BODY" ]] || return 1
  ! grep -Eiq 'aio_key|secret|api_key' "$HTTP_BODY" || return 1

  if [[ "$HOST_ID" == h0000000 ]]; then
    other_host_id=h0000001
  else
    other_host_id=h0000000
  fi
  request_gateway "$other_host_id.$HOST_SUFFIX" /__ch/api/host "$COOKIE_CONFIG"
  [[ "$HTTP_CODE" == 404 ]]
}

if command -v go >/dev/null 2>&1 && [[ -d "$SERVER_DIR/internal/gateway" ]]; then
  if gateway_unit_tests; then
    pass T04.14 'local Gateway tests cover cookie scope, ticket replay, fixed 404, credential stripping, body limits, and WebSocket behavior'
  else
    status=$?
    if ((status == 2)); then
      skip T04.14 'Go or Gateway package is unavailable'
    else
      fail T04.14 'local Gateway security and boundary tests' 'one or more targeted tests failed'
    fi
  fi
else
  skip T04.14 'Go or server/internal/gateway is unavailable'
fi

if [[ -z "$GATEWAY_URL" ]]; then
  skip T04.15 'set CH_VERIFY_GATEWAY_URL to run read-only local Gateway HTTP checks'
elif ! valid_loopback_url "$GATEWAY_URL"; then
  fail T04.15 'Gateway target is a literal loopback HTTP origin' 'no request was sent; only http://127.0.0.1 or http://[::1] origins are allowed'
elif ! valid_host_suffix "$HOST_SUFFIX" || ! valid_host_id "$HOST_ID"; then
  skip T04.15 'HTTP checks also require CH_VERIFY_HOST_SUFFIX and CH_VERIFY_HOST_ID'
elif ! command -v curl >/dev/null 2>&1; then
  skip T04.15 'curl is unavailable; no Gateway HTTP request was sent'
else
  GATEWAY_URL="${GATEWAY_URL%/}"
  if request_gateway "$HOST_ID.$HOST_SUFFIX" /__ch/api/host && [[ "$HTTP_CODE" != 000 ]]; then
    pass T04.15 'configured loopback Gateway answered a read-only request'
    if check_gateway_listener; then
      pass A04.01/A04.02 'unauthenticated and malformed Host requests return the same fixed 404'
    else
      fail A04.01/A04.02 'unauthenticated and malformed Host requests return the same fixed 404' 'unexpected status or response body'
    fi
    if check_denied_preview_ports; then
      pass A04.11 'preview ports below the allowlist and port 8080 are rejected'
    else
      fail A04.11 'preview ports below the allowlist and port 8080 are rejected' 'unexpected status'
    fi

    if [[ -n "$PRIVATE_PORT" ]]; then
      if valid_port "$PRIVATE_PORT" && check_private_preview_redirect; then
        pass A04.11 'private preview without a Host Cookie redirects to the authenticated preview flow'
      else
        fail A04.11 'private preview without a Host Cookie redirects to the authenticated preview flow' 'configured port or redirect was invalid'
      fi
    else
      skip A04.11 'set CH_VERIFY_PRIVATE_PREVIEW_PORT to check the unauthenticated private-preview redirect'
    fi

    if [[ -n "$HOST_COOKIE" ]]; then
      if make_cookie_config && check_cookie_authorization; then
        pass A04.03 'configured Host Cookie authorizes only its Host and exposes no credential fields'
      else
        fail A04.03 'configured Host Cookie authorizes only its Host and exposes no credential fields' 'cookie rejected or scope check failed'
      fi
    else
      skip A04.03 'set CH_VERIFY_HOST_COOKIE for a read-only Host-cookie authorization check; cookie attributes are covered by Go tests'
    fi
  else
    skip T04.15 'configured loopback Gateway is not reachable; no host or preview probes were sent'
  fi
fi

verify_finish
