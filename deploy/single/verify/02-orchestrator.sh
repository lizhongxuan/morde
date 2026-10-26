#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "$VERIFY_DIR/../../.." && pwd)"
SERVER_DIR="$ROOT_DIR/server"
OSB_URL="${CH_OSB_URL:-}"
OSB_API_KEY="${CH_OSB_API_KEY:-}"

skip() {
  printf 'SKIP %s %s\n' "$1" "$2"
}

run_local_tests() {
  command -v go >/dev/null 2>&1 || return 2
  (
    cd -- "$SERVER_DIR"
    env -u CH_OSB_URL -u CH_OSB_API_KEY -u CH_TEST_DATABASE_URL \
      GOPROXY=off GOTOOLCHAIN=local \
      go test -count=1 ./internal/config ./internal/domain/template ./internal/orchestrator
  )
}

is_loopback_osb_url() {
  local url="$1" port
  [[ "$url" =~ ^https?://(127\.0\.0\.1|\[::1\])(:([0-9]{1,5}))?(/[^[:space:]?#]*)?$ ]] || return 1
  port="${BASH_REMATCH[3]:-}"
  [[ -z "$port" ]] || ((10#$port > 0 && 10#$port <= 65535))
}

run_osb_list_integration() (
  cd -- "$SERVER_DIR"
  unset CH_TEST_DATABASE_URL
  export CH_OSB_URL="$OSB_URL"
  export CH_OSB_API_KEY="$OSB_API_KEY"
  export GOPROXY=off GOTOOLCHAIN=local
  go test -count=1 -timeout=45s -run '^TestOpenSandboxIntegrationList$' ./internal/orchestrator
)

if [[ ! -d "$SERVER_DIR" ]]; then
  fail T02.11 'local config/template/orchestrator tests' 'server source directory is unavailable'
elif run_local_tests; then
  pass T02.11 'config, template, and OpenSandbox adapter unit tests passed (including golden, error, and metrics checks)'
else
  status=$?
  if ((status == 2)); then
    skip T02.11 'Go is unavailable; local package tests were not run'
  else
    fail T02.11 'config, template, and OpenSandbox adapter unit tests' 'one or more tests failed'
  fi
fi

if [[ -n "$OSB_URL" ]] && ! is_loopback_osb_url "$OSB_URL"; then
  fail T02.12 'read-only OpenSandbox List integration' 'CH_OSB_URL must use a literal 127.0.0.1 or [::1] address; no request was sent'
elif [[ -z "$OSB_URL" || -z "$OSB_API_KEY" ]]; then
  skip T02.12 'set both CH_OSB_URL and CH_OSB_API_KEY with a loopback URL to run the existing read-only OpenSandbox List integration test'
elif ! command -v go >/dev/null 2>&1; then
  skip T02.12 'Go is unavailable; OpenSandbox integration was not run'
elif check T02.12 'existing TestOpenSandboxIntegrationList passed against the configured loopback OpenSandbox server' run_osb_list_integration; then
  :
fi

verify_finish
