#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "$0")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
ROOT_DIR="$(cd -- "$VERIFY_DIR/../../.." && pwd)"

run_project_gates() {
  local project_dir="$1"
  (
    cd -- "$project_dir"
    npm run lint &&
      npm run typecheck &&
      npm test &&
      npm run build
  )
}

run_mock_e2e() {
  local project_dir="$1"
  (
    cd -- "$project_dir"
    env -u CH_REAL_E2E -u CH_PORTAL_URL -u CH_USERNAME -u CH_PASSWORD \
      npm run e2e -- --grep "mock"
  )
}

check A05.01 'portal lint, typecheck, unit tests, and production build' run_project_gates "$ROOT_DIR/web/portal" || true
check A05.01 'workbench lint, typecheck, unit tests, and production build' run_project_gates "$ROOT_DIR/web/workbench" || true
# $1 belongs to the inner bash process.
# shellcheck disable=SC2016
check A05.02 'shell quoting unit tests, including injection filenames' bash -c 'cd "$1" && npm test -- src/util/shellQuote.test.ts' _ "$ROOT_DIR/web/workbench" || true
check A05.03/A05.04 'portal mock lifecycle and bounded enter clicks' run_mock_e2e "$ROOT_DIR/web/portal" || true
# shellcheck disable=SC2016
check A05.03/A05.10/A05.12 'workbench mock file and tab flows' bash -c 'cd "$1" && env -u CH_REAL_E2E -u CH_PORTAL_URL -u CH_USERNAME -u CH_PASSWORD npm run e2e' _ "$ROOT_DIR/web/workbench" || true

real_run="$(printenv CH_REAL_E2E 2>/dev/null || true)"
if [[ "$real_run" == "1" ]]; then
  portal_url="$(printenv CH_PORTAL_URL 2>/dev/null || true)"
  username="$(printenv CH_USERNAME 2>/dev/null || true)"
  password="$(printenv CH_PASSWORD 2>/dev/null || true)"
  if [[ -z "$portal_url" || -z "$username" || -z "$password" ]]; then
    fail A05.05/A05.06 'real single-host flow' 'CH_PORTAL_URL, CH_USERNAME, and CH_PASSWORD are required with CH_REAL_E2E=1'
  elif [[ ! "$portal_url" =~ ^https?://(localhost|127\.0\.0\.1|\[::1\]|[a-zA-Z0-9.-]+\.localhost)(:[0-9]+)?(/|$) ]]; then
    fail A05.05/A05.06 'real single-host flow' 'only loopback and *.localhost targets are allowed by the local-only workspace policy'
  else
    # shellcheck disable=SC2016
    check A05.05/A05.06 'real single-host create, file check, and cleanup flow' \
      bash -c 'cd "$1" && npm run e2e -- --grep "real single-host flow"' _ "$ROOT_DIR/web/portal" || true
  fi
else
  printf 'SKIP A05.05/A05.06 real-host tests require explicit CH_REAL_E2E=1 and credentials\n'
fi

verify_finish
