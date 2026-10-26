#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(git -C "$VERIFY_DIR/../../.." rev-parse --show-toplevel)"
SERVER_DIR="$ROOT_DIR/server"
PORTAL_BIN="${CH_VERIFY_PORTAL_BIN:-/opt/cloudhost/bin/portal}"
PORTAL_CONFIG="${CH_VERIFY_PORTAL_CONFIG:-/etc/cloudhost/portal.yaml}"
API_URL="${CH_VERIFY_API_URL:-http://127.0.0.1:7080}"
METRICS_URL="${CH_VERIFY_METRICS_URL:-http://127.0.0.1:7082/metrics}"

run_redocly() {
  command -v npx >/dev/null 2>&1 || return 1
  (cd -- "$SERVER_DIR" && npx --yes @redocly/cli@2.64.1 lint openapi/portal.yaml)
}

run_api_tests() {
  local output="$VERIFY_TMPDIR/api-tests.log" percent
  if (cd -- "$SERVER_DIR" && go test -cover ./internal/api/...) >"$output" 2>&1; then
    cat -- "$output"
  else
    cat -- "$output"
    return 1
  fi
  percent="$(awk '{for (i = 1; i < NF; i++) if ($i == "coverage:") {gsub(/%/, "", $(i + 1)); print $(i + 1); exit}}' "$output")"
  [[ -n "$percent" ]] && awk -v value="$percent" 'BEGIN {exit !(value >= 80)}'
}

run_quality_gates() {
  local generated="$SERVER_DIR/internal/store/postgres/sqlcgen" before after
  [[ -d "$generated" ]] || return 1
  before="$(find "$generated" -type f -exec cksum {} + | LC_ALL=C sort)"
  (cd -- "$SERVER_DIR" && make lint test race cover generate) || return 1
  after="$(find "$generated" -type f -exec cksum {} + | LC_ALL=C sort)"
  [[ "$before" == "$after" ]]
}

loopback_url() {
  [[ "$1" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]{1,5})?$ ]]
}

health_checks() {
  local health ready
  loopback_url "$API_URL" || return 1
  health="$(curl_code "${API_URL%/}/healthz")"
  ready="$(curl_code "${API_URL%/}/readyz")"
  [[ "$health" == 200 && "$ready" == 200 ]]
}

metrics_checks() {
  local body="$VERIFY_TMPDIR/metrics.txt"
  loopback_url "${METRICS_URL%/metrics}" || return 1
  curl --silent --show-error --connect-timeout 5 --max-time 15 --output "$body" "$METRICS_URL" || return 1
  grep -q '^cloudhost_http_requests_total' "$body" || return 1
  grep -q '^cloudhost_hosts{state=' "$body" || return 1
  ! grep -Eq '"[hut][a-z0-9]{7}"' "$body"
}

run_migration_check() {
  local before after tables
  [[ "${CH_VERIFY_ALLOW_DB_MUTATION:-}" == yes && -n "${CH_DATABASE_DSN:-}" ]] || return 2
  [[ -x "$PORTAL_BIN" ]] || return 1
  command -v psql >/dev/null 2>&1 || return 1
  "$PORTAL_BIN" migrate up --config "$PORTAL_CONFIG" || return 1
  before="$(PGDATABASE="$CH_DATABASE_DSN" psql -XAtqc 'SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY version_id DESC LIMIT 1')" || return 1
  tables="$(PGDATABASE="$CH_DATABASE_DSN" psql -XAtqc "SELECT count(*) FROM unnest(ARRAY['users','workspaces','hosts','tokens','audit_logs']) AS t(name) WHERE to_regclass(name) IS NOT NULL")" || return 1
  [[ "$tables" == 5 ]] || return 1
  "$PORTAL_BIN" migrate up --config "$PORTAL_CONFIG" || return 1
  after="$(PGDATABASE="$CH_DATABASE_DSN" psql -XAtqc 'SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY version_id DESC LIMIT 1')" || return 1
  [[ "$before" == "$after" ]]
}

check A03.01 'OpenAPI contract lint' run_redocly || true
check A03.20 'Portal API tests pass with at least 80% coverage' run_api_tests || true
check A03.30 'server lint, tests, race, coverage, and generation gates' run_quality_gates || true

if [[ "${CH_VERIFY_ALLOW_DB_MUTATION:-}" == yes && -n "${CH_DATABASE_DSN:-}" ]]; then
  check A03.02 'migration is idempotent and all five application tables exist' run_migration_check || true
else
  printf 'SKIP A03.02 migration check requires CH_VERIFY_ALLOW_DB_MUTATION=yes and an explicitly supplied CH_DATABASE_DSN; it changes the selected database\n'
fi

if loopback_url "$API_URL" && [[ "$(curl_code "${API_URL%/}/healthz")" != 000 ]]; then
  check A03.19 'local healthz and readyz checks' health_checks || true
else
  printf 'SKIP A03.19 local Portal API is not reachable at the loopback CH_VERIFY_API_URL\n'
fi

if loopback_url "${METRICS_URL%/metrics}" && [[ "$(curl_code "$METRICS_URL")" != 000 ]]; then
  check A03.28 'local metrics are present and labels do not contain user, host, or token IDs' metrics_checks || true
else
  printf 'SKIP A03.28 local metrics endpoint is not reachable at CH_VERIFY_METRICS_URL\n'
fi

printf 'SKIP A03.03-A03.18/A03.21-A03.27/A03.29/A03.31-A03.32 require disposable accounts, a dedicated Portal database/OpenSandbox instance, or manual/chaos setup; this script will not mutate those targets by default\n'
verify_finish
