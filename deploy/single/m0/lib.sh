#!/usr/bin/env bash

set -Eeuo pipefail

CH_AIO_IMAGE="${CH_AIO_IMAGE:-ghcr.io/agent-infra/sandbox:1.11.0@sha256:6328d7fd2f0ff0b4c147c3d05b3df1ce331f4a482eb6e550ecd64ed1fcf906e7}"
CH_AIO_HOST="${CH_AIO_HOST:-127.0.0.1}"
CH_AIO_PORT="${CH_AIO_PORT:-8080}"
CH_AIO_URL="${CH_AIO_URL:-http://${CH_AIO_HOST}:${CH_AIO_PORT}}"
CH_AIO_SHM_SIZE="${CH_AIO_SHM_SIZE:-2g}"
CH_AIO_START_TIMEOUT="${CH_AIO_START_TIMEOUT:-180}"
CH_OSB_URL="${CH_OSB_URL:-http://127.0.0.1:18080}"
CH_OSB_API_KEY="${CH_OSB_API_KEY:-}"
CH_OSB_REQUEST_TIMEOUT="${CH_OSB_REQUEST_TIMEOUT:-30}"
CH_OSB_READY_TIMEOUT="${CH_OSB_READY_TIMEOUT:-180}"
CH_OSB_ENDPOINT_SCHEME="${CH_OSB_ENDPOINT_SCHEME:-http}"
CH_M0_CURL_TIMEOUT="${CH_M0_CURL_TIMEOUT:-15}"
CH_M0_POLL_INTERVAL="${CH_M0_POLL_INTERVAL:-2}"
CH_EXPECT_OSB_DOCKER_RUNTIME="${CH_EXPECT_OSB_DOCKER_RUNTIME:-runsc}"
CH_EXPECTED_PIDS_LIMIT="${CH_EXPECTED_PIDS_LIMIT:-4096}"

CH_AIO_URL="${CH_AIO_URL%/}"
CH_OSB_URL="${CH_OSB_URL%/}"

M0_TMPDIR=""
M0_CONTAINER_NAME=""
M0_AIO_KEY=""
M0_HTTP_CODE=""
M0_LAST_SANDBOX_ID=""
M0_LAST_ENDPOINT_URL=""
M0_LAST_CREATED_CONTAINER=""
M0_BEFORE_IMAGE_CONTAINERS=""
M0_FAILURES=0
M0_SANDBOX_IDS=()
M0_CREATED_VOLUMES=()

m0_note() { printf 'INFO %s\n' "$*"; }
m0_pass() { printf 'PASS %s\n' "$*"; }
m0_fail() { M0_FAILURES=$((M0_FAILURES + 1)); printf 'FAIL %s\n' "$*" >&2; }

m0_fatal() {
  printf 'ERROR %s\n' "$*" >&2
  exit 1
}

m0_require() {
  local command_name
  for command_name in "$@"; do
    command -v "$command_name" >/dev/null 2>&1 || m0_fatal "required command is missing: $command_name"
  done
}

m0_begin() {
  local task_name="$1"
  local timeout_value
  for timeout_value in "$CH_AIO_START_TIMEOUT" "$CH_OSB_REQUEST_TIMEOUT" "$CH_OSB_READY_TIMEOUT" "$CH_M0_CURL_TIMEOUT" "$CH_M0_POLL_INTERVAL"; do
    [[ "$timeout_value" =~ ^[1-9][0-9]*$ ]] || m0_fatal 'M0 timeout and poll settings must be positive whole seconds'
  done
  umask 077
  M0_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/cloudhost-${task_name}.XXXXXX")" || m0_fatal "cannot create private temp directory"
  chmod 700 "$M0_TMPDIR"
  trap m0_on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

m0_on_exit() {
  local status=$?
  trap - EXIT INT TERM
  if ! m0_cleanup; then
    status=1
  fi
  exit "$status"
}

m0_cleanup() {
  local status=0 sandbox_id volume_name active_containers

  for sandbox_id in "${M0_SANDBOX_IDS[@]-}"; do
    [[ -n "$sandbox_id" ]] || continue
    if ! m0_osb_delete "$sandbox_id"; then
      printf 'FAIL cleanup sandbox=%s (kept related volumes)\n' "$sandbox_id" >&2
      status=1
    fi
  done

  if command -v docker >/dev/null 2>&1; then
    for volume_name in "${M0_CREATED_VOLUMES[@]-}"; do
      [[ -n "$volume_name" ]] || continue
      active_containers="$(docker ps -aq --filter "volume=$volume_name" 2>/dev/null || true)"
      if [[ -n "$active_containers" ]]; then
        printf 'FAIL cleanup volume=%s still has container references; volume left intact\n' "$volume_name" >&2
        status=1
      elif ! docker volume rm "$volume_name" >/dev/null 2>&1; then
        printf 'FAIL cleanup volume=%s could not be removed\n' "$volume_name" >&2
        status=1
      fi
    done

    if [[ -n "$M0_CONTAINER_NAME" ]] && docker container inspect "$M0_CONTAINER_NAME" >/dev/null 2>&1; then
      if ! docker rm -f "$M0_CONTAINER_NAME" >/dev/null 2>&1; then
        printf 'FAIL cleanup container=%s could not be removed\n' "$M0_CONTAINER_NAME" >&2
        status=1
      fi
    fi
  fi

  if [[ -n "$M0_TMPDIR" && -d "$M0_TMPDIR" ]]; then
    rm -rf -- "$M0_TMPDIR" || status=1
  fi
  return "$status"
}

m0_require_linux_docker() {
  [[ "$(uname -s)" == Linux ]] || m0_fatal 'these M0 runtime checks require Linux'
  m0_require docker curl jq
  docker info >/dev/null 2>&1 || m0_fatal 'Docker daemon is unavailable'
}

m0_require_osb() {
  m0_require curl jq docker
  [[ -n "$CH_OSB_API_KEY" ]] || m0_fatal 'set CH_OSB_API_KEY; the key is never printed by this script'
  [[ "$CH_OSB_API_KEY" != *$'\n'* && "$CH_OSB_API_KEY" != *$'\r'* ]] || m0_fatal 'CH_OSB_API_KEY must be a single-line value'
}

m0_generate_aio_key() {
  if [[ -n "${CH_AIO_API_KEY:-}" ]]; then
    M0_AIO_KEY="$CH_AIO_API_KEY"
  else
    m0_require openssl
    M0_AIO_KEY="$(openssl rand -hex 32)"
  fi
  [[ "$M0_AIO_KEY" != *$'\n'* && "$M0_AIO_KEY" != *$'\r'* ]] || m0_fatal 'CH_AIO_API_KEY must be a single-line value'
}

m0_write_curl_config() {
  local header_name="$1" value="$2" escaped
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  escaped="${value//\\/\\\\}"
  escaped="${escaped//\"/\\\"}"
  printf 'header = "%s: %s"\n' "$header_name" "$escaped" > "$M0_TMPDIR/curl.cfg"
  chmod 600 "$M0_TMPDIR/curl.cfg"
}

m0_write_curl_config_url() {
  local value="$1" escaped
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  escaped="${value//\\/\\\\}"
  escaped="${escaped//\"/\\\"}"
  printf 'url = "%s"\n' "$escaped" > "$M0_TMPDIR/curl.cfg"
  chmod 600 "$M0_TMPDIR/curl.cfg"
}

# Usage: m0_request METHOD URL OUTPUT AUTH [JSON_BODY]
# AUTH is one of: none, aio, aio-bearer, aio-query, aio-mcp, osb.
m0_request() {
  local method="$1" url="$2" output_file="$3" auth="$4" body="${5:-}"
  local config_file="$M0_TMPDIR/curl.cfg" body_file="$M0_TMPDIR/request.json" curl_error="$M0_TMPDIR/curl.err" status
  local curl_url="$url"
  local -a args

  args=(--silent --show-error --connect-timeout 5 --max-time "$CH_M0_CURL_TIMEOUT" --request "$method" --output "$output_file" --write-out '%{http_code}')
  if [[ -n "$body" ]]; then
    printf '%s' "$body" > "$body_file"
    chmod 600 "$body_file"
    args+=(--header 'Content-Type: application/json' --data-binary "@$body_file")
  fi

  case "$auth" in
    none) rm -f -- "$config_file" ;;
    aio)
      m0_write_curl_config X-AIO-API-Key "$M0_AIO_KEY" || return 1
      args+=(--config "$config_file")
      ;;
    aio-bearer)
      m0_write_curl_config Authorization "Bearer $M0_AIO_KEY" || return 1
      args+=(--config "$config_file")
      ;;
    aio-query)
      if [[ "$url" == *\?* ]]; then
        url="${url}&api_key=$(jq -nr --arg key "$M0_AIO_KEY" '$key | @uri')"
      else
        url="${url}?api_key=$(jq -nr --arg key "$M0_AIO_KEY" '$key | @uri')"
      fi
      m0_write_curl_config_url "$url" || return 1
      args+=(--config "$config_file")
      curl_url=''
      ;;
    aio-mcp)
      m0_write_curl_config X-AIO-API-Key "$M0_AIO_KEY" || return 1
      args+=(--config "$config_file" --header 'Accept: application/json, text/event-stream')
      ;;
    osb)
      m0_write_curl_config OPEN-SANDBOX-API-KEY "$CH_OSB_API_KEY" || return 1
      args+=(--config "$config_file")
      ;;
    *) m0_fatal "unknown authentication mode: $auth" ;;
  esac

  if [[ -n "$curl_url" ]]; then
    if ! status="$(curl "${args[@]}" "$curl_url" 2>"$curl_error")"; then
      M0_HTTP_CODE=000
      return 1
    fi
  else
    if ! status="$(curl "${args[@]}" 2>"$curl_error")"; then
      M0_HTTP_CODE=000
      return 1
    fi
  fi
  M0_HTTP_CODE="$status"
  [[ "$M0_HTTP_CODE" =~ ^[0-9]{3}$ ]]
}

m0_response_success() {
  local code="$1"
  [[ "$code" =~ ^2[0-9][0-9]$ ]]
}

m0_aio_status() {
  local path="$1" auth="${2:-aio}"
  if m0_request GET "$CH_AIO_URL$path" "$M0_TMPDIR/response" "$auth"; then
    printf '%s' "$M0_HTTP_CODE"
  else
    printf '000'
  fi
}

m0_aio_wait_ready() {
  local base_url="$1" max_seconds="${2:-$CH_AIO_START_TIMEOUT}" attempt status
  local attempts=$((max_seconds / CH_M0_POLL_INTERVAL + 1))
  for ((attempt = 0; attempt < attempts; attempt++)); do
    if m0_request GET "$base_url/v1/shell/sessions" "$M0_TMPDIR/response" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
      return 0
    fi
    sleep "$CH_M0_POLL_INTERVAL"
  done
  status="$M0_HTTP_CODE"
  m0_fail "AIO readiness timed out after ${max_seconds}s (last HTTP $status)"
  return 1
}

m0_aio_exec() {
  local base_url="$1" command_text="$2" auth="${3:-aio}" timeout="${4:-10}" hard_timeout="${5:-15}"
  local body
  body="$(jq -cn --arg command "$command_text" --argjson timeout "$timeout" --argjson hard_timeout "$hard_timeout" '{command:$command,timeout:$timeout,hard_timeout:$hard_timeout,truncate:false}')"
  m0_request POST "$base_url/v1/shell/exec" "$M0_TMPDIR/response" "$auth" "$body"
}

m0_aio_json() {
  local base_url="$1" method="$2" path="$3" body="${4:-}" auth="${5:-aio}"
  m0_request "$method" "$base_url$path" "$M0_TMPDIR/response" "$auth" "$body"
}

m0_aio_exec_output() {
  jq -r '.data.output // empty' "$M0_TMPDIR/response"
}

m0_aio_check_surface() {
  local base_url="${1:-$CH_AIO_URL}" check_id="${2:-A01.03}" screenshot_file="$M0_TMPDIR/screenshot.png" signature mcp_body browser_body

  if m0_request GET "$base_url/v1/docs" "$M0_TMPDIR/response" aio && m0_response_success "$M0_HTTP_CODE"; then
    m0_pass "$check_id AIO docs endpoint responds"
  else
    m0_fail "$check_id AIO docs endpoint HTTP $M0_HTTP_CODE"
  fi

  if m0_aio_exec "$base_url" 'printf cloudhost-m0-ok' && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.success == true and .data.status == "completed" and .data.exit_code == 0 and ((.data.output // "") | contains("cloudhost-m0-ok"))' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass "$check_id AIO shell command returned expected output"
  else
    m0_fail "$check_id AIO shell command failed (HTTP $M0_HTTP_CODE)"
  fi

  browser_body="$(jq -cn '{url:"data:text/html,<main>cloudhost-m0-browser-ok</main>",wait_until:"domcontentloaded",timeout:20}')"
  if m0_aio_json "$base_url" POST /v1/browser/page/navigate "$browser_body" && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
    browser_body="$(jq -cn '{expression:"document.querySelector(\"main\")?.textContent"}')"
    if m0_aio_json "$base_url" POST /v1/browser/page/evaluate "$browser_body" && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.success == true and .data == "cloudhost-m0-browser-ok"' "$M0_TMPDIR/response" >/dev/null; then
      m0_pass "$check_id AIO browser navigated and read page text"
    else
      m0_fail "$check_id AIO browser page interaction failed (HTTP $M0_HTTP_CODE)"
    fi
  else
    m0_fail "$check_id AIO browser navigation failed (HTTP $M0_HTTP_CODE)"
  fi

  if m0_request GET "$base_url/v1/browser/screenshot" "$screenshot_file" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
    signature="$(od -An -tx1 -N8 "$screenshot_file" | tr -d ' \n')"
    if [[ "$signature" == 89504e470d0a1a0a ]]; then
      m0_pass "$check_id AIO browser screenshot is a PNG"
    else
      m0_fail "$check_id AIO screenshot response was not a PNG"
    fi
  else
    m0_fail "$check_id AIO screenshot failed (HTTP $M0_HTTP_CODE)"
  fi

  if m0_request GET "$base_url/vnc/index.html?autoconnect=true" "$M0_TMPDIR/response" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
    m0_pass "$check_id AIO VNC browser page responds"
  else
    m0_fail "$check_id AIO VNC page failed (HTTP $M0_HTTP_CODE)"
  fi

  mcp_body='{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
  if m0_request POST "$base_url/mcp" "$M0_TMPDIR/mcp-response" aio-mcp "$mcp_body" && m0_response_success "$M0_HTTP_CODE"; then
    if jq -e '.jsonrpc == "2.0" and (.result.tools | type == "array")' "$M0_TMPDIR/mcp-response" >/dev/null 2>&1 \
      || sed -n 's/^data: *//p' "$M0_TMPDIR/mcp-response" | jq -e '.jsonrpc == "2.0" and (.result.tools | type == "array")' >/dev/null 2>&1; then
      m0_pass "$check_id AIO MCP tools/list responds"
    else
      m0_fail "$check_id AIO MCP endpoint returned an invalid tools/list response"
    fi
  else
    m0_fail "$check_id AIO MCP tools/list failed (HTTP $M0_HTTP_CODE)"
  fi
}

m0_start_aio_container() {
  local runtime="$1" runtime_args=()
  m0_require_linux_docker
  m0_generate_aio_key
  if [[ "$runtime" == runsc ]]; then
    docker info --format '{{json .Runtimes}}' | jq -e 'has("runsc")' >/dev/null || m0_fatal 'Docker does not register runsc'
    runtime_args=(--runtime=runsc)
  elif [[ "$runtime" != runc ]]; then
    m0_fatal "unsupported test runtime: $runtime"
  fi
  M0_CONTAINER_NAME="ch-m0-aio-${runtime}-$(date +%s)-$$"
  docker container inspect "$M0_CONTAINER_NAME" >/dev/null 2>&1 && m0_fatal "container name already exists: $M0_CONTAINER_NAME"
  docker image inspect "$CH_AIO_IMAGE" >/dev/null 2>&1 || docker pull "$CH_AIO_IMAGE" >/dev/null || m0_fatal 'could not pull configured AIO image'

  export SANDBOX_API_KEY="$M0_AIO_KEY"
  docker run --detach --name "$M0_CONTAINER_NAME" "${runtime_args[@]}" \
    --security-opt seccomp=unconfined --shm-size "$CH_AIO_SHM_SIZE" \
    --publish "$CH_AIO_HOST:$CH_AIO_PORT:8080" --env SANDBOX_API_KEY \
    "$CH_AIO_IMAGE" >/dev/null || m0_fatal 'could not start AIO container'
  m0_aio_wait_ready "$CH_AIO_URL" || m0_fatal 'AIO did not become ready'
}

m0_require_pinned_aio_image() {
  [[ "$CH_AIO_IMAGE" =~ @sha256:[[:xdigit:]]{64}$ ]] || m0_fatal 'CH_AIO_IMAGE must include a complete sha256 digest'
}

m0_require_osb_and_linux() {
  m0_require_linux_docker
  m0_require_osb
  [[ "$CH_OSB_ENDPOINT_SCHEME" == http || "$CH_OSB_ENDPOINT_SCHEME" == https ]] || m0_fatal 'CH_OSB_ENDPOINT_SCHEME must be http or https'
  local policy_script
  policy_script="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)/network-policy.sh"
  bash "$policy_script" check || m0_fatal 'host network isolation check failed; OpenSandbox create was not attempted'
  if ! m0_request GET "$CH_OSB_URL/health" "$M0_TMPDIR/response" none || ! m0_response_success "$M0_HTTP_CODE"; then
    m0_fatal "OpenSandbox health check failed (HTTP $M0_HTTP_CODE)"
  fi
}

m0_osb_json() {
  local method="$1" path="$2" body="${3:-}"
  local previous_timeout="$CH_M0_CURL_TIMEOUT" status
  CH_M0_CURL_TIMEOUT="$CH_OSB_REQUEST_TIMEOUT"
  if m0_request "$method" "$CH_OSB_URL/v1$path" "$M0_TMPDIR/response" osb "$body"; then
    status=0
  else
    status=$?
  fi
  CH_M0_CURL_TIMEOUT="$previous_timeout"
  return "$status"
}

m0_osb_create() {
  local body="$1" id
  m0_osb_json POST /sandboxes "$body" || return 1
  m0_response_success "$M0_HTTP_CODE" || return 1
  id="$(jq -er '.id' "$M0_TMPDIR/response")" || return 1
  # shellcheck disable=SC2034
  M0_LAST_SANDBOX_ID="$id"
  M0_SANDBOX_IDS+=("$id")
}

m0_osb_state() {
  local sandbox_id="$1"
  if m0_osb_json GET "/sandboxes/$sandbox_id" && [[ "$M0_HTTP_CODE" == 200 ]]; then
    jq -r '.status.state // empty' "$M0_TMPDIR/response"
  else
    printf 'HTTP_%s' "$M0_HTTP_CODE"
  fi
}

m0_osb_wait_state() {
  local sandbox_id="$1" expected_state="$2" max_seconds="${3:-$CH_OSB_READY_TIMEOUT}" attempt state
  local attempts=$((max_seconds / CH_M0_POLL_INTERVAL + 1))
  for ((attempt = 0; attempt < attempts; attempt++)); do
    state="$(m0_osb_state "$sandbox_id")"
    if [[ "$state" == "$expected_state" ]]; then
      return 0
    fi
    case "$state" in
      Failed|Terminated|Stopping|HTTP_401|HTTP_403|HTTP_404) return 1 ;;
    esac
    sleep "$CH_M0_POLL_INTERVAL"
  done
  return 1
}

m0_osb_delete() {
  local sandbox_id="$1" attempt state
  [[ -n "$sandbox_id" ]] || return 0
  if ! m0_osb_json DELETE "/sandboxes/$sandbox_id"; then
    return 1
  fi
  if [[ "$M0_HTTP_CODE" != 200 && "$M0_HTTP_CODE" != 202 && "$M0_HTTP_CODE" != 204 && "$M0_HTTP_CODE" != 404 ]]; then
    return 1
  fi
  for ((attempt = 0; attempt < 60; attempt++)); do
    if ! m0_osb_json GET "/sandboxes/$sandbox_id"; then
      return 1
    fi
    if [[ "$M0_HTTP_CODE" == 404 ]]; then
      return 0
    fi
    state="$(jq -r '.status.state // empty' "$M0_TMPDIR/response" 2>/dev/null || true)"
    if [[ "$state" == Terminated ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

m0_osb_endpoint() {
  local sandbox_id="$1" port="${2:-8080}" raw_endpoint
  m0_osb_json GET "/sandboxes/$sandbox_id/endpoints/$port" || return 1
  [[ "$M0_HTTP_CODE" == 200 ]] || return 1
  raw_endpoint="$(jq -er '.endpoint' "$M0_TMPDIR/response")" || return 1
  if [[ "$raw_endpoint" == http://* || "$raw_endpoint" == https://* ]]; then
    # shellcheck disable=SC2034
    M0_LAST_ENDPOINT_URL="${raw_endpoint%/}"
  else
    # shellcheck disable=SC2034
    M0_LAST_ENDPOINT_URL="${CH_OSB_ENDPOINT_SCHEME}://${raw_endpoint%/}"
  fi
}

m0_capture_aio_container_ids() {
  M0_BEFORE_IMAGE_CONTAINERS="$(docker ps --quiet --filter "ancestor=$CH_AIO_IMAGE" | tr '\n' ' ')"
}

m0_check_created_sandbox_container() {
  local container_id found="" count=0 effective_runtime configured_default network_mode cap_drop status=0
  for container_id in $(docker ps --quiet --filter "ancestor=$CH_AIO_IMAGE"); do
    case " $M0_BEFORE_IMAGE_CONTAINERS " in
      *" $container_id "*) ;;
      *) found="$container_id"; count=$((count + 1)) ;;
    esac
  done
  [[ "$count" == 1 ]] || { m0_fail "expected one new Docker container for the sandbox, found $count"; return 1; }

  # shellcheck disable=SC2034
  M0_LAST_CREATED_CONTAINER="$found"
  effective_runtime="$(docker inspect --format '{{.HostConfig.Runtime}}' "$found")"
  configured_default="$(docker info --format '{{.DefaultRuntime}}')"
  [[ -n "$effective_runtime" ]] || effective_runtime="$configured_default"
  network_mode="$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$found")"
  cap_drop="$(docker inspect --format '{{json .HostConfig.CapDrop}}' "$found")"

  if [[ "$effective_runtime" == "$CH_EXPECT_OSB_DOCKER_RUNTIME" ]]; then
    m0_pass "sandbox Docker runtime=$effective_runtime"
  else
    m0_fail "sandbox Docker runtime expected=$CH_EXPECT_OSB_DOCKER_RUNTIME actual=${effective_runtime:-unknown}"
    status=1
  fi
  if [[ "$network_mode" == cloudhost ]]; then
    m0_pass 'sandbox Docker network mode=cloudhost'
  else
    m0_fail "sandbox Docker network mode expected=cloudhost actual=$network_mode"
    status=1
  fi
  if jq -e 'type == "array" and index("NET_RAW") != null' <<<"$cap_drop" >/dev/null; then
    m0_pass 'sandbox Docker drops NET_RAW'
  else
    m0_fail "sandbox Docker CapDrop omitted NET_RAW (actual=${cap_drop:-unknown})"
    status=1
  fi

  return "$status"
}

m0_create_aio_sandbox_body() {
  local volume_name="${1:-}" metadata_value="${M0_RUN_ID:-m0}" volume_size="${CH_TEST_VOLUME_SIZE:-20Gi}"
  local body
  if [[ -n "$volume_name" ]]; then
    body="$(jq -cn --arg image "$CH_AIO_IMAGE" --arg key "$M0_AIO_KEY" --arg volume "$volume_name" --arg size "$volume_size" --arg run "$metadata_value" \
      '{image:{uri:$image},entrypoint:["/opt/gem/run.sh"],timeout:300,resourceLimits:{cpu:"2",memory:"4Gi"},env:{SANDBOX_API_KEY:$key},metadata:{"m0-run":$run},volumes:[{name:"workspace",pvc:{claimName:$volume,storage:$size,createIfNotExists:true,deleteOnSandboxTermination:false},mountPath:"/home/gem/workspace"}]}')"
  else
    body="$(jq -cn --arg image "$CH_AIO_IMAGE" --arg key "$M0_AIO_KEY" --arg run "$metadata_value" \
      '{image:{uri:$image},entrypoint:["/opt/gem/run.sh"],timeout:300,resourceLimits:{cpu:"2",memory:"4Gi"},env:{SANDBOX_API_KEY:$key},metadata:{"m0-run":$run}}')"
  fi
  printf '%s' "$body"
}

m0_record_elapsed() {
  local started="$1"
  printf '%ss' "$((SECONDS - started))"
}

m0_finish() {
  if ((M0_FAILURES > 0)); then
    m0_note "${M0_FAILURES} check(s) failed"
    return 1
  fi
  return 0
}
