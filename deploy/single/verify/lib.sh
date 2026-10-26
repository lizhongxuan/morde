#!/usr/bin/env bash

set -Eeuo pipefail

VERIFY_TMPDIR=""
VERIFY_FAILURES=0
VERIFY_CLEANUPS=()

pass() {
  printf 'PASS %s %s\n' "$1" "$2"
}

fail() {
  VERIFY_FAILURES=$((VERIFY_FAILURES + 1))
  printf 'FAIL %s %s %s\n' "$1" "$2" "${3:-check failed}" >&2
}

verify_begin() {
  umask 077
  VERIFY_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/cloudhost-verify.XXXXXX")"
  chmod 700 "$VERIFY_TMPDIR"
  trap verify_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

verify_register_cleanup() {
  [[ "$1" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || return 2
  declare -F "$1" >/dev/null || return 2
  VERIFY_CLEANUPS+=("$1")
}

verify_cleanup() {
  local index cleanup status=0
  for ((index = ${#VERIFY_CLEANUPS[@]} - 1; index >= 0; index--)); do
    cleanup="${VERIFY_CLEANUPS[index]}"
    if ! "$cleanup"; then
      fail CLEANUP "$cleanup" 'cleanup failed'
      status=1
    fi
  done
  if [[ -n "$VERIFY_TMPDIR" && -d "$VERIFY_TMPDIR" ]]; then
    rm -rf -- "$VERIFY_TMPDIR" || {
      fail CLEANUP temporary-files 'could not remove the private temporary directory'
      status=1
    }
    VERIFY_TMPDIR=""
  fi
  return "$status"
}

verify_on_exit() {
  local status=$?
  trap - EXIT INT TERM
  if ! verify_cleanup; then
    status=1
  fi
  ((VERIFY_FAILURES == 0)) || status=1
  exit "$status"
}

check() {
  local check_id="$1" description="$2" status
  shift 2
  if "$@"; then
    pass "$check_id" "$description"
  else
    status=$?
    fail "$check_id" "$description" "command exited $status"
    return "$status"
  fi
}

curl_code() {
  local url="$1" output_file="${VERIFY_TMPDIR:-${TMPDIR:-/tmp}}/curl-body"
  local -a args=(--silent --show-error --connect-timeout "${VERIFY_CONNECT_TIMEOUT:-5}" --max-time "${VERIFY_REQUEST_TIMEOUT:-15}" --output "$output_file" --write-out '%{http_code}')
  if [[ -n "${VERIFY_CURL_CONFIG:-}" ]]; then
    args+=(--config "$VERIFY_CURL_CONFIG")
  fi
  curl "${args[@]}" "$url" 2>/dev/null || printf '000'
}

curl_status() {
  local expected="$1" url="$2" actual
  actual="$(curl_code "$url")"
  [[ "$actual" == "$expected" ]]
}

wait_for() {
  local timeout="$1" interval="$2" elapsed=0 sleep_time
  shift 2
  [[ "$timeout" =~ ^[0-9]+$ && "$interval" =~ ^[1-9][0-9]*$ ]] || return 2
  while ((elapsed <= timeout)); do
    if "$@"; then
      return 0
    fi
    ((elapsed == timeout)) && break
    sleep_time="$interval"
    ((elapsed + sleep_time > timeout)) && sleep_time=$((timeout - elapsed))
    sleep "$sleep_time"
    elapsed=$((elapsed + sleep_time))
  done
  return 1
}

verify_run_script() {
  local check_id="$1" description="$2" script_path="$3"
  if bash "$script_path"; then
    pass "$check_id" "$description"
  else
    fail "$check_id" "$description" 'see the M0 script output above'
    return 1
  fi
}

verify_finish() {
  ((VERIFY_FAILURES == 0))
}
