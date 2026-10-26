#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "$VERIFY_DIR/../../.." && pwd)"
SERVER_DIR="$ROOT_DIR/server"
IS_LINUX=no
[[ "$(uname -s 2>/dev/null || true)" == Linux ]] && IS_LINUX=yes

skip() {
  printf 'SKIP %s %s\n' "$1" "$2"
}

security_unit_tests() {
  local pkg pattern
  command -v go >/dev/null 2>&1 || return 2
  local -a cases=(
    './internal/gateway|^(TestTicketCookieControlRoutesAndFixedNotFound|TestCookieAuthHostAPIAndLogout|TestMissingAndWrongScopeCredentialsReturnSameNotFound|TestProxyStripsPlatformCredentialsAndInjectsAIOKey|TestCredentialCookieTamperExpiryAndScope|TestWebSocketOriginAndTrackedConnection|TestCookieWritesRejectMissingAndCrossHostOrigins|TestRetiredEndpointsAreNotProxied)$'
    './internal/app|^(TestCredentialServiceLoginVerifyLogoutAndRateLimit|TestCredentialServiceTicketConsumesOnceAndPropagatesStoreErrors|TestAuditServiceValidatesAndRedactsBeforeWrite|TestHostServiceAdminListAndOwnershipHideForeignHosts|TestHostServiceCreateExistingWorkspaceChecksOwnerAndState)$'
    './internal/httpserver|^(TestBaseRequestIDLogAndMetrics|TestWriteErrorBodyRetryAfterAndLogOnce|TestRecoveryHidesPanicAndFinalizesRequest|TestSetRouteUsesFixedLabelAndFullPath)$'
    './internal/api|^(TestAuthRequiredAndApplicationErrorsAreSafe|TestDomainErrorResponsesAreMappedAndSanitized|TestLoginMeAndLogoutKeepCredentialOutOfJSON|TestInvalidRequestsAndOriginAreRejected)$'
    './internal/mcp|^(TestOutputLimitsAndSafeErrors|TestToolErrorsPanicAndAuditFailure|TestStreamableHTTPSessionUsesCurrentAPIKeyPrincipal|TestMCPBehindLoopbackProxyValidatesHostAndOrigin)$'
  )
  for entry in "${cases[@]}"; do
    pkg="${entry%%|*}"
    pattern="${entry#*|}"
    (cd -- "$SERVER_DIR" && env -u CH_TEST_DATABASE_URL -u CH_OSB_URL -u CH_OSB_API_KEY \
      GOPROXY=off GOTOOLCHAIN=local go test -count=1 -run "$pattern" "$pkg") || return 1
  done
}

check_listeners() {
  local raw="$1" port output addresses address
  local -a ports
  IFS=',' read -r -a ports <<<"$raw"
  ((${#ports[@]} > 0)) || return 2
  output="$(ss -lntH 2>/dev/null)" || return 1
  for port in "${ports[@]}"; do
    port="${port//[[:space:]]/}"
    [[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port > 0 && 10#$port <= 65535)) || return 2
    addresses="$(awk -v wanted="$port" 'NF >= 4 {local=$4; n=split(local, part, ":"); if (part[n] == wanted) print local}' <<<"$output")"
    [[ -n "$addresses" ]] || return 1
    while IFS= read -r address; do
      case "$address" in
        "127.0.0.1:$port"|"[::1]:$port"|"::1:$port") ;;
        *) return 1 ;;
      esac
    done <<<"$addresses"
  done
}

check_key_file() {
  local var_name="$1" path mode size uid
  path="${!var_name:-}"
  [[ -n "$path" ]] || return 2
  [[ -f "$path" && ! -L "$path" ]] || return 1
  mode="$(stat -c '%a' -- "$path" 2>/dev/null)" || return 1
  size="$(wc -c <"$path" | tr -d '[:space:]')"
  uid="$(stat -c '%u' -- "$path" 2>/dev/null)" || return 1
  [[ "$mode" == 600 && "$size" == 32 && "$uid" =~ ^[0-9]+$ ]] || return 1
  if [[ -n "${CH_VERIFY_KEY_OWNER_UID:-}" ]]; then
    [[ "$CH_VERIFY_KEY_OWNER_UID" =~ ^[0-9]+$ && "$uid" == "$CH_VERIFY_KEY_OWNER_UID" ]] || return 1
  fi
}

docker_ready() {
  local socket="$1"
  command -v docker >/dev/null 2>&1 && [[ -S "$socket" ]] || return 1
  docker --host "unix://$socket" info --format '{{.ServerVersion}}' >/dev/null 2>&1
}

check_docker_socket_permissions() {
  local socket="$1" mode uid
  [[ -S "$socket" ]] || return 1
  mode="$(stat -c '%a' -- "$socket" 2>/dev/null)" || return 1
  uid="$(stat -c '%u' -- "$socket" 2>/dev/null)" || return 1
  [[ "$uid" == 0 && ( "$mode" == 600 || "$mode" == 660 ) ]]
}

check_osb_docker_socket_mount() {
  local socket="$1" osb_container="$2" inspect osb_id running mount socket_real mount_real
  local id mounts source container_ids
  [[ -n "$osb_container" && "$osb_container" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || return 2
  command -v readlink >/dev/null 2>&1 || return 2
  socket_real="$(readlink -f -- "$socket" 2>/dev/null)" || return 1
  inspect="$(docker --host "unix://$socket" inspect --format '{{.Id}}|{{.State.Running}}|{{range .Mounts}}{{if eq .Destination "/var/run/docker.sock"}}{{.Source}}{{end}}{{end}}' "$osb_container" 2>/dev/null)" || return 1
  IFS='|' read -r osb_id running mount <<<"$inspect"
  [[ "$running" == true && -n "$mount" ]] || return 1
  mount_real="$(readlink -f -- "$mount" 2>/dev/null)" || return 1
  [[ "$mount_real" == "$socket_real" ]] || return 1

  container_ids="$(docker --host "unix://$socket" ps --no-trunc --quiet 2>/dev/null)" || return 1
  while IFS= read -r id; do
    [[ -n "$id" && "$id" != "$osb_id" ]] || continue
    mounts="$(docker --host "unix://$socket" inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' "$id" 2>/dev/null)" || return 1
    while IFS= read -r source; do
      [[ -n "$source" && -e "$source" ]] || continue
      source="$(readlink -f -- "$source" 2>/dev/null || true)"
      [[ "$source" != "$socket_real" ]] || return 1
    done <<<"$mounts"
  done <<<"$container_ids"
}

check_running_sandbox_limits() {
  local socket="$1" container="$2" expected_max="$3" expected_cpu_max="$4"
  local inspect running runtime pids memory nano_cpus cpu_quota cpu_period
  [[ -n "$container" && "$container" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || return 2
  [[ "$expected_max" =~ ^[0-9]+$ ]] && ((10#$expected_max > 0)) || return 2
  [[ "$expected_cpu_max" =~ ^[0-9]+([.][0-9]+)?$ ]] && awk -v value="$expected_cpu_max" 'BEGIN { exit !(value > 0) }' || return 2
  inspect="$(docker --host "unix://$socket" inspect --format '{{.State.Running}}|{{.HostConfig.Runtime}}|{{.HostConfig.PidsLimit}}|{{.HostConfig.Memory}}|{{.HostConfig.NanoCpus}}|{{.HostConfig.CpuQuota}}|{{.HostConfig.CpuPeriod}}' "$container" 2>/dev/null)" || return 1
  IFS='|' read -r running runtime pids memory nano_cpus cpu_quota cpu_period <<<"$inspect"
  [[ "$running" == true && "$runtime" == runsc && "$pids" == 4096 && "$memory" =~ ^[0-9]+$ ]] || return 1
  ((10#$memory > 0 && 10#$memory <= 10#$expected_max)) || return 1
  cpu_limit_within_max "$nano_cpus" "$cpu_quota" "$cpu_period" "$expected_cpu_max"
}

network_policy_check() {
  local policy_script="/opt/cloudhost/network-policy.sh" service_ids
  service_ids="$(docker --host "unix://$DOCKER_SOCKET" ps --quiet \
    --filter 'label=com.docker.compose.project=cloudhost' \
    --filter 'label=com.docker.compose.service=opensandbox-server' 2>/dev/null)" || {
    fail T07.13 'local cloudhost network policy check' 'could not inspect the local OpenSandbox service'
    return 0
  }
  if [[ -z "$service_ids" ]]; then
    skip T07.13 'no running cloudhost OpenSandbox service; network policy check skipped, no PASS claimed'
  elif [[ ! -x "$policy_script" ]]; then
    fail T07.13 'local cloudhost network policy check' 'running OpenSandbox service requires executable /opt/cloudhost/network-policy.sh'
  elif ((EUID != 0)) && { ! command -v sudo >/dev/null 2>&1 || ! sudo -n true >/dev/null 2>&1; }; then
    skip T07.13 'root or passwordless sudo is unavailable for nftables inspection; no PASS claimed'
  elif DOCKER_HOST="unix://$DOCKER_SOCKET" "$policy_script" check; then
    pass T07.13 'fixed cloudhost bridge, IPv6 disablement, NET_RAW drop, and nftables policy fingerprint verified'
  else
    fail T07.13 'fixed cloudhost bridge, IPv6 disablement, NET_RAW drop, and nftables policy fingerprint verified' 'network-policy.sh check failed'
  fi
}

cpu_limit_within_max() {
  local nano_cpus="$1" cpu_quota="$2" cpu_period="$3" expected_max="$4"
  [[ "$nano_cpus" =~ ^[0-9]+$ && "$cpu_quota" =~ ^[0-9]+$ && "$cpu_period" =~ ^[0-9]+$ ]] || return 1
  awk -v nano="$nano_cpus" -v quota="$cpu_quota" -v period="$cpu_period" -v maximum="$expected_max" 'BEGIN {
    if (nano > 0) exit !(nano / 1000000000 <= maximum)
    if (quota > 0 && period > 0) exit !(quota / period <= maximum)
    exit 1
  }'
}

if command -v go >/dev/null 2>&1 && [[ -d "$SERVER_DIR" ]]; then
  if security_unit_tests; then
    pass T07.01 'targeted local tests cover auth, cookie/ticket scope, owner checks, log/audit redaction, safe errors, and proxy credential stripping'
  else
    status=$?
    if ((status == 2)); then
      skip T07.01 'Go or server packages are unavailable'
    else
      fail T07.01 'targeted local security tests' 'one or more tests failed'
    fi
  fi
else
  skip T07.01 'Go or server source is unavailable'
fi

LISTEN_PORTS="${CH_VERIFY_LISTEN_PORTS:-}"
if [[ -z "$LISTEN_PORTS" ]]; then
  skip T07.04 'set CH_VERIFY_LISTEN_PORTS to inspect explicitly configured local service ports'
elif [[ "$IS_LINUX" != yes ]]; then
  skip T07.04 'host listener inspection requires Linux and ss'
elif ! command -v ss >/dev/null 2>&1; then
  skip T07.04 'ss is unavailable; no listener result is claimed'
elif check_listeners "$LISTEN_PORTS"; then
  pass T07.04 'all configured service listeners are present and bound only to loopback'
else
  status=$?
  if ((status == 2)); then
    fail T07.04 'configured listener port list is valid' 'invalid CH_VERIFY_LISTEN_PORTS value'
  else
    fail T07.04 'all configured service listeners are present and bound only to loopback' 'a listener is missing or bound outside loopback'
  fi
fi

DATA_KEY_FILE="${CH_VERIFY_DATA_KEY_FILE:-}"
COOKIE_KEY_FILE="${CH_VERIFY_COOKIE_KEY_FILE:-}"
if [[ "$IS_LINUX" != yes ]]; then
  skip T07.09 'key file permission checks require Linux stat semantics'
else
  if [[ -z "$DATA_KEY_FILE" ]]; then
    skip T07.09 'set CH_VERIFY_DATA_KEY_FILE to inspect the configured data key file'
  elif check_key_file CH_VERIFY_DATA_KEY_FILE; then
    pass T07.09 'configured data key is a regular 32-byte file with mode 0600 and matches the owner UID if configured'
  else
    fail T07.09 'configured data key is a regular 32-byte file with mode 0600' 'file is missing, linked, wrong-sized, or has unsafe permissions/owner'
  fi
  if [[ -z "$COOKIE_KEY_FILE" ]]; then
    skip T07.09 'set CH_VERIFY_COOKIE_KEY_FILE to inspect the configured cookie key file'
  elif check_key_file CH_VERIFY_COOKIE_KEY_FILE; then
    pass T07.09 'configured cookie key is a regular 32-byte file with mode 0600 and matches the owner UID if configured'
  else
    fail T07.09 'configured cookie key is a regular 32-byte file with mode 0600' 'file is missing, linked, wrong-sized, or has unsafe permissions/owner'
  fi
  if [[ -z "${CH_VERIFY_KEY_OWNER_UID:-}" ]]; then
    skip T07.09 'set CH_VERIFY_KEY_OWNER_UID if the Portal service owner must be enforced'
  fi
fi

DOCKER_SOCKET="${CH_VERIFY_DOCKER_SOCKET:-}"
OSB_CONTAINER="${CH_VERIFY_OSB_CONTAINER:-}"
SANDBOX_CONTAINER="${CH_VERIFY_SANDBOX_CONTAINER:-}"
MAX_MEMORY_BYTES="${CH_VERIFY_MAX_MEMORY_BYTES:-4294967296}"
MAX_CPUS="${CH_VERIFY_MAX_CPUS:-2}"
if [[ "$IS_LINUX" != yes ]]; then
  skip T07.12 'Docker socket and container inspection requires Linux'
  skip T07.02/T07.05 'running Sandbox runtime/resource inspection requires Linux'
  skip T07.13 'host network policy inspection requires Linux; no PASS claimed'
elif [[ -z "$DOCKER_SOCKET" ]]; then
  skip T07.12 'set CH_VERIFY_DOCKER_SOCKET to inspect the local Docker socket and its mounts'
  skip T07.02/T07.05 'set CH_VERIFY_DOCKER_SOCKET and CH_VERIFY_SANDBOX_CONTAINER for read-only Sandbox inspection'
  skip T07.13 'set CH_VERIFY_DOCKER_SOCKET to inspect local network policy; no PASS claimed'
elif [[ "$DOCKER_SOCKET" != /* || "$DOCKER_SOCKET" == *".."* || "$DOCKER_SOCKET" == *$'\n'* ]]; then
  fail T07.12 'Docker inspection uses an explicit local Unix socket path' 'no Docker command was sent'
  fail T07.02/T07.05 'Docker inspection uses an explicit local Unix socket path' 'no Docker command was sent'
  skip T07.13 'configured Docker socket is not a valid local path; network policy check skipped, no PASS claimed'
elif ! docker_ready "$DOCKER_SOCKET"; then
  skip T07.12 'configured local Docker socket or Docker CLI is unavailable'
  skip T07.02/T07.05 'configured local Docker socket or Docker CLI is unavailable'
  skip T07.13 'configured local Docker socket or Docker CLI is unavailable; no PASS claimed'
else
  if check_docker_socket_permissions "$DOCKER_SOCKET"; then
    pass T07.12 'Docker socket is root-owned and mode 0600 or 0660'
  else
    fail T07.12 'Docker socket is root-owned and mode 0600 or 0660' 'socket ownership or permissions are unsafe'
  fi

  if [[ -z "$OSB_CONTAINER" ]]; then
    skip T07.12 'set CH_VERIFY_OSB_CONTAINER to verify the running OpenSandbox container socket mount'
  elif check_osb_docker_socket_mount "$DOCKER_SOCKET" "$OSB_CONTAINER"; then
    pass T07.12 'running OpenSandbox container mounts the configured Docker socket and no other running container does'
  else
    status=$?
    if ((status == 2)); then
      skip T07.12 'OpenSandbox container ID/name or local path resolver is invalid/unavailable'
    else
      fail T07.12 'running OpenSandbox container mounts the configured Docker socket and no other running container does' 'mount or container ownership check failed'
    fi
  fi

  if [[ -z "$SANDBOX_CONTAINER" ]]; then
    skip T07.02/T07.05 'set CH_VERIFY_SANDBOX_CONTAINER to inspect one running AIO Sandbox'
  elif check_running_sandbox_limits "$DOCKER_SOCKET" "$SANDBOX_CONTAINER" "$MAX_MEMORY_BYTES" "$MAX_CPUS"; then
    pass T07.02/T07.05 'configured running Sandbox uses runsc, pids_limit=4096, and finite CPU/memory limits within the configured maxima'
  else
    status=$?
    if ((status == 2)); then
      fail T07.02/T07.05 'Sandbox inspection inputs are valid' 'container name or CPU/memory limit is invalid'
    else
      fail T07.02/T07.05 'configured running Sandbox uses runsc, pids_limit=4096, and finite CPU/memory limits within the configured maxima' 'runtime/state/resource check failed'
    fi
  fi

  network_policy_check
fi

skip T07.03 'egress isolation requires an isolated Sandbox probe and is not inferred from config or local unit tests'
skip T07.06/T07.07/T07.08 'cross-user browser/Origin and ticket replay behavior is covered by targeted local tests; live identity setup is not performed'
skip T07.10/T07.11 'audit coverage is exercised by local tests; live Portal headers and end-to-end audit flow need an explicitly prepared local stack'

verify_finish
