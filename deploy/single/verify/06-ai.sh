#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

REAL_RUN="${CH_REAL_E2E:-0}"
PORTAL_URL="${CH_PORTAL_URL:-}"
HOST_SUFFIX="${CH_HOST_SUFFIX:-}"
SCHEME="${CH_SCHEME:-https}"
TEMPLATE="${CH_TEMPLATE:-std-2c4g}"
MCP_READY_TIMEOUT=180
MCP_POLL_INTERVAL=2
WORKSPACE_READY_TIMEOUT=180
WORKSPACE_POLL_INTERVAL=2
MCP_MAX_OUTPUT_BYTES=65536
MCP_SCREENSHOT_MAX_WIDTH=1280
WORKSPACE="/home/gem/workspace"
BROWSER_FIXTURE="${WORKSPACE%/}/.mcp-browser-${$}.html"
FILE_FIXTURE_NAME=".mcp-verify-${$}.txt"
FILE_FIXTURE_CONTENT="mcp-verify-${$}-content"
PLATFORM_URL=""
PLATFORM_AUTH=""
COOKIE_JAR=""
API_KEY_ID=""
PLATFORM_SESSION=""
PLATFORM_PROTOCOL_VERSION=""
CURRENT_USER_ID=""
OWN_HOST_ID=""
OWN_HOST_URL=""
OWN_WORKSPACE_ID=""
HOST_DELETE_ACCEPTED=0
MCP_RPC_ID=0
MCP_STATUS=""
MCP_JSON="${VERIFY_TMPDIR}/mcp-response.json"
MCP_HEADERS="${VERIFY_TMPDIR}/mcp-headers"
MCP_PAYLOAD="${VERIFY_TMPDIR}/mcp-payload.json"
API_STATUS=""
API_JSON="${VERIFY_TMPDIR}/api-response.json"
RUN_STARTED="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LIFECYCLE_OK=1
VERIFY_RESOURCE_SUFFIX="$(date -u '+%Y%m%d%H%M%S')-${$}-${RANDOM}"
API_KEY_NAME="verify-ai-key-${VERIFY_RESOURCE_SUFFIX}"
OWN_HOST_NAME="verify-ai-host-${VERIFY_RESOURCE_SUFFIX}"
API_KEY_CREATE_ATTEMPTED=0
HOST_CREATE_ATTEMPTED=0

url_is_local() {
  [[ "$1" =~ ^https?://(localhost|127\.0\.0\.1|\[::1\]|([A-Za-z0-9-]+\.)*localhost)(:[0-9]{1,5})?/?$ ]]
}

write_bearer_config() {
  local token="$1" path="$2" prefix="$3"
  [[ "$token" =~ ^${prefix}_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+$ ]] || return 1
  printf 'header = "Authorization: Bearer %s"\n' "$token" >"$path"
  chmod 600 "$path"
}

portal_request() {
  local method="$1" path="$2" body_file="${3:-}" idempotency_key="${4:-}" output_file status
  output_file="${VERIFY_TMPDIR}/api-$((MCP_RPC_ID + 1)).json"
  local -a args=(--silent --connect-timeout "${VERIFY_CONNECT_TIMEOUT:-5}" --max-time "${VERIFY_REQUEST_TIMEOUT:-30}" --request "$method" --cookie "$COOKIE_JAR" --cookie-jar "$COOKIE_JAR" --output "$output_file" --write-out '%{http_code}')
  case "$method" in
    GET|HEAD) ;;
    *) args+=(--header 'Content-Type: application/json' --header "Origin: ${PORTAL_URL%/}") ;;
  esac
  [[ -z "$idempotency_key" ]] || args+=(--header "Idempotency-Key: ${idempotency_key}")
  if [[ -n "$body_file" ]]; then
    args+=(--data-binary "@$body_file")
  fi
  status="$(curl "${args[@]}" "${PORTAL_URL}${path}" 2>/dev/null || true)"
  API_STATUS="${status:-000}"
  API_JSON="$output_file"
}

mcp_post() {
  local url="$1" auth_config="$2" session_id="$3" request_file="$4" protocol_version="${5:-}" output_file status
  output_file="${VERIFY_TMPDIR}/mcp-raw.json"
  local -a args=(--silent --connect-timeout "${VERIFY_CONNECT_TIMEOUT:-5}" --max-time "${VERIFY_REQUEST_TIMEOUT:-30}" --request POST --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' --data-binary "@$request_file" --dump-header "$MCP_HEADERS" --output "$output_file" --write-out '%{http_code}')
  [[ -z "$auth_config" ]] || args+=(--config "$auth_config")
  [[ -z "$session_id" ]] || args+=(--header "Mcp-Session-Id: $session_id")
  [[ -z "$protocol_version" ]] || args+=(--header "MCP-Protocol-Version: $protocol_version")
  status="$(curl "${args[@]}" "$url" 2>/dev/null || true)"
  MCP_STATUS="${status:-000}"
  if jq -e . "$output_file" >/dev/null 2>&1; then
    cp -- "$output_file" "$MCP_JSON"
  elif ! awk '/^data: / {sub(/^data: /, ""); print}' "$output_file" | tail -n 1 >"$MCP_JSON" || ! jq -e . "$MCP_JSON" >/dev/null 2>&1; then
    cp -- "$output_file" "$MCP_JSON"
  fi
}

rpc_request_url() {
  local url="$1" auth_config="$2" session_id="$3" method="$4" params="$5" request_file request_id protocol_version=""
  MCP_RPC_ID=$((MCP_RPC_ID + 1))
  request_id="$MCP_RPC_ID"
  request_file="${VERIFY_TMPDIR}/mcp-request.json"
  jq -cn --argjson id "$request_id" --arg method "$method" --argjson params "$params" \
    '{jsonrpc:"2.0",id:$id,method:$method,params:$params}' >"$request_file"
  [[ "$method" == initialize ]] || protocol_version="$PLATFORM_PROTOCOL_VERSION"
  mcp_post "$url" "$auth_config" "$session_id" "$request_file" "$protocol_version"
  [[ "$MCP_STATUS" == 200 ]] || return 1
  jq -e --argjson id "$request_id" '.jsonrpc == "2.0" and .id == $id and (has("result") or has("error"))' "$MCP_JSON" >/dev/null
}

mcp_tool_call() {
  local name="$1" arguments="$2" params
  params="$(jq -cn --arg name "$name" --argjson arguments "$arguments" '{name:$name,arguments:$arguments}')"
  rpc_request_url "$PLATFORM_URL" "$PLATFORM_AUTH" "$PLATFORM_SESSION" tools/call "$params" || return 1
  jq -e 'has("result")' "$MCP_JSON" >/dev/null
}

mcp_result_ok() {
  jq -e 'has("result") and (.result.isError != true) and (has("error") | not)' "$MCP_JSON" >/dev/null
}

mcp_extract_payload() {
  jq -e '
    if (.result.structuredContent? != null) then .result.structuredContent
    elif (.result.content? | type) == "array" then
      ([.result.content[]? | select(.type == "text") | .text] | join("\n")) as $text
      | try ($text | fromjson) catch {text:$text}
    else .result
    end
  ' "$MCP_JSON" >"$MCP_PAYLOAD"
}

payload_text() {
  jq -r 'if type == "string" then . elif (.markdown|type)=="string" then .markdown elif (.content|type)=="string" then .content elif (.text|type)=="string" then .text elif (.stdout|type)=="string" then .stdout elif (.output|type)=="string" then .output else tostring end' "$MCP_PAYLOAD"
}

setup_login_and_key() {
  local login_file key_file
  [[ -n "${CH_ADMIN_USER:-}" && -n "${CH_ADMIN_PASSWORD:-}" ]] || return 1
  COOKIE_JAR="${VERIFY_TMPDIR}/portal-cookies"
  : >"$COOKIE_JAR"
  chmod 600 "$COOKIE_JAR"
  login_file="${VERIFY_TMPDIR}/login.json"
  python3 - "$login_file" <<'PY'
import json
import os
import sys

with open(sys.argv[1], "w", encoding="utf-8") as output:
    json.dump({"name": os.environ["CH_ADMIN_USER"], "password": os.environ["CH_ADMIN_PASSWORD"]}, output)
PY
  portal_request POST /api/auth/login "$login_file"
  rm -f -- "$login_file"
  unset CH_ADMIN_PASSWORD
  [[ "$API_STATUS" == 200 ]] || return 1
  CURRENT_USER_ID="$(jq -r '.user.id // empty' "$API_JSON")"
  [[ -n "$CURRENT_USER_ID" ]] || return 1
  portal_request GET /api/me
  [[ "$API_STATUS" == 200 ]] && jq -e '.role == "admin"' "$API_JSON" >/dev/null || return 1

  if [[ -n "${CH_API_KEY:-}" ]]; then
    PLATFORM_AUTH="${VERIFY_TMPDIR}/platform-auth.conf"
    if ! write_bearer_config "$CH_API_KEY" "$PLATFORM_AUTH" chk; then
      unset CH_API_KEY
      return 1
    fi
    unset CH_API_KEY
  else
    key_file="${VERIFY_TMPDIR}/apikey.json"
    portal_request GET /api/apikeys
    [[ "$API_STATUS" == 200 ]] && jq -e --arg name "$API_KEY_NAME" '(.items | type) == "array" and ([.items[] | select(.name == $name)] | length == 0)' "$API_JSON" >/dev/null || return 1
    jq -cn --arg name "$API_KEY_NAME" '{name:$name,ttl_seconds:3600}' >"$key_file"
    API_KEY_CREATE_ATTEMPTED=1
    portal_request POST /api/apikeys "$key_file"
    rm -f -- "$key_file"
    [[ "$API_STATUS" == 201 ]] || return 1
    API_KEY_ID="$(jq -r '.id // empty' "$API_JSON")"
    local api_key
    api_key="$(jq -r '.key // empty' "$API_JSON")"
    [[ "$API_KEY_ID" =~ ^t[a-z0-9]{7}$ && -n "$api_key" ]] || return 1
    PLATFORM_AUTH="${VERIFY_TMPDIR}/platform-auth.conf"
    if ! write_bearer_config "$api_key" "$PLATFORM_AUTH" chk; then
      api_key=""
      return 1
    fi
    api_key=""
    rm -f -- "$API_JSON"
  fi
  return 0
}

resource_id_by_name() {
  local path="$1" name="$2"
  portal_request GET "$path"
  [[ "$API_STATUS" == 200 ]] || return 1
  jq -e '(.items | type) == "array"' "$API_JSON" >/dev/null || return 1
  jq -r --arg name "$name" '[.items[]? | select(.name == $name) | .id | select(type == "string")] | if length == 1 then .[0] else "" end' "$API_JSON"
}

cleanup_created_host() {
  local resolved
  ((HOST_CREATE_ATTEMPTED == 1)) || return 0
  if [[ ! "$OWN_HOST_ID" =~ ^h[a-z0-9]{7}$ ]]; then
    resolved="$(resource_id_by_name /api/hosts "$OWN_HOST_NAME")" || return 1
    OWN_HOST_ID="$resolved"
  fi
  [[ -n "$OWN_HOST_ID" ]] || return 0
  [[ "$OWN_HOST_ID" =~ ^h[a-z0-9]{7}$ ]] || return 1
  ((HOST_DELETE_ACCEPTED == 0)) || return 0
  portal_request DELETE "/api/hosts/${OWN_HOST_ID}?keepWorkspace=false"
  [[ "$API_STATUS" == 200 || "$API_STATUS" == 202 || "$API_STATUS" == 404 ]] || return 1
  HOST_DELETE_ACCEPTED=1
}

cleanup_created_workspace() {
  local elapsed=0 count row state attached_host_id
  [[ "$OWN_WORKSPACE_ID" =~ ^w[a-z0-9]{7}$ ]] || return 0
  while ((elapsed <= WORKSPACE_READY_TIMEOUT)); do
    portal_request GET /api/workspaces
    [[ "$API_STATUS" == 200 ]] || return 1
    jq -e '(.items | type) == "array"' "$API_JSON" >/dev/null || return 1
    count="$(jq --arg id "$OWN_WORKSPACE_ID" '[.items[]? | select(.id == $id)] | length' "$API_JSON")"
    [[ "$count" =~ ^[0-9]+$ ]] || return 1
    if [[ "$count" == 0 ]]; then
      return 0
    elif [[ "$count" != 1 ]]; then
      return 1
    fi
    row="$(jq -c --arg id "$OWN_WORKSPACE_ID" '.items[] | select(.id == $id)' "$API_JSON")"
    state="$(jq -r '.state // empty' <<<"$row")"
    attached_host_id="$(jq -r '.attached_host_id // empty' <<<"$row")"
    case "$state" in
      deleting|deleted) return 0 ;;
      detached)
        [[ -z "$attached_host_id" ]] || return 1
        portal_request DELETE "/api/workspaces/${OWN_WORKSPACE_ID}"
        [[ "$API_STATUS" == 202 || "$API_STATUS" == 404 ]] && return 0
        return 1
        ;;
      attached)
        [[ -n "$attached_host_id" && "$HOST_DELETE_ACCEPTED" == 1 ]] || return 1
        ;;
      *) return 1 ;;
    esac
    ((elapsed == WORKSPACE_READY_TIMEOUT)) && break
    sleep "$WORKSPACE_POLL_INTERVAL"
    elapsed=$((elapsed + WORKSPACE_POLL_INTERVAL))
  done
  return 1
}

cleanup_created_api_key() {
  local resolved
  ((API_KEY_CREATE_ATTEMPTED == 1)) || return 0
  if [[ ! "$API_KEY_ID" =~ ^t[a-z0-9]{7}$ ]]; then
    resolved="$(resource_id_by_name /api/apikeys "$API_KEY_NAME")" || return 1
    API_KEY_ID="$resolved"
  fi
  [[ -n "$API_KEY_ID" ]] || return 0
  [[ "$API_KEY_ID" =~ ^t[a-z0-9]{7}$ ]] || return 1
  portal_request DELETE "/api/apikeys/${API_KEY_ID}"
  [[ "$API_STATUS" == 204 || "$API_STATUS" == 404 ]]
}

verify_register_cleanup cleanup_created_workspace
verify_register_cleanup cleanup_created_host
verify_register_cleanup cleanup_created_api_key

validate_real_environment() {
  [[ "$REAL_RUN" == 1 && -n "$PORTAL_URL" && -n "$HOST_SUFFIX" ]] || return 1
  url_is_local "$PORTAL_URL" || return 1
  [[ "$HOST_SUFFIX" =~ ^([A-Za-z0-9-]+\.)*localhost$ ]] || return 1
  [[ "$SCHEME" == http || "$SCHEME" == https ]] || return 1
  [[ "$TEMPLATE" =~ ^[a-z0-9-]{1,32}$ ]] || return 1
  command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1 || return 1
  [[ -n "${CH_ADMIN_USER:-}" && -n "${CH_ADMIN_PASSWORD:-}" ]] || return 1
  PLATFORM_URL="${PORTAL_URL%/}/mcp"
}

test_unauthenticated() {
  local request_file bad_auth status_ok=1
  request_file="${VERIFY_TMPDIR}/auth-request.json"
  jq -cn '{jsonrpc:"2.0",id:1,method:"initialize",params:{protocolVersion:"2025-03-26",capabilities:{},clientInfo:{name:"cloudhost-verify",version:"1"}}}' >"$request_file"
  bad_auth="${VERIFY_TMPDIR}/bad-auth.conf"
  printf 'header = "Authorization: Bearer chk_invalid_invalid"\n' >"$bad_auth"
  chmod 600 "$bad_auth"
  mcp_post "$PLATFORM_URL" "" "" "$request_file"
  if [[ "$MCP_STATUS" != 401 ]] || ! jq -e 'tostring | contains("unauthenticated")' "$MCP_JSON" >/dev/null 2>&1; then status_ok=0; fi
  mcp_post "$PLATFORM_URL" "$bad_auth" "" "$request_file"
  if [[ "$MCP_STATUS" != 401 ]] || ! jq -e 'tostring | contains("unauthenticated")' "$MCP_JSON" >/dev/null 2>&1; then status_ok=0; fi
  ((status_ok == 1))
}

initialize_platform() {
  local params
  params='{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cloudhost-verify","version":"1"}}'
  rpc_request_url "$PLATFORM_URL" "$PLATFORM_AUTH" "" initialize "$params" || return 1
  jq -e '.result.protocolVersion != null and .result.capabilities != null' "$MCP_JSON" >/dev/null || return 1
  PLATFORM_PROTOCOL_VERSION="$(jq -r '.result.protocolVersion' "$MCP_JSON")"
  [[ "$PLATFORM_PROTOCOL_VERSION" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || return 1
  PLATFORM_SESSION="$(awk 'tolower($1)=="mcp-session-id:" {gsub("\r", "", $2); value=$2} END {print value}' "$MCP_HEADERS")"
  [[ -z "$PLATFORM_SESSION" || "$PLATFORM_SESSION" =~ ^[A-Za-z0-9._-]+$ ]]
}

list_all_tools() {
  local cursor="" params chunk tools='[]' page
  for ((page = 0; page < 20; page++)); do
    if [[ -n "$cursor" ]]; then
      params="$(jq -cn --arg cursor "$cursor" '{cursor:$cursor}')"
    else
      params='{}'
    fi
    rpc_request_url "$PLATFORM_URL" "$PLATFORM_AUTH" "$PLATFORM_SESSION" tools/list "$params" || return 1
    chunk="$(jq -ce '.result.tools // []' "$MCP_JSON")" || return 1
    tools="$(jq -cn --argjson current "$tools" --argjson next "$chunk" '$current + $next')"
    cursor="$(jq -r '.result.nextCursor // empty' "$MCP_JSON")"
    [[ -n "$cursor" ]] || break
  done
  [[ -z "$cursor" ]] || return 1
  printf '%s\n' "$tools" >"${VERIFY_TMPDIR}/tools.json"
}

validate_tool_schemas() {
  local expected="${VERIFY_TMPDIR}/expected-tools.json"
  cat >"$expected" <<'JSON'
[
 {"name":"host_create","required":["template_id"],"properties":{"template_id":"string","name":"string","workspace":"object"}},
 {"name":"host_list","required":[],"properties":{}},
 {"name":"host_get","required":["host_id"],"properties":{"host_id":"string"}},
 {"name":"host_delete","required":["host_id"],"properties":{"host_id":"string","keep_workspace":"boolean"}},
 {"name":"host_renew","required":["host_id"],"properties":{"host_id":"string"}},
 {"name":"shell_exec","required":["host_id","command"],"properties":{"host_id":"string","command":"string","timeout":"integer","cwd":"string"}},
 {"name":"file_read","required":["host_id","path"],"properties":{"host_id":"string","path":"string","start_line":"integer","end_line":"integer"}},
 {"name":"file_write","required":["host_id","path","content"],"properties":{"host_id":"string","path":"string","content":"string","append":"boolean"}},
 {"name":"file_list","required":["host_id","path"],"properties":{"host_id":"string","path":"string","recursive":"boolean","show_hidden":"boolean"}},
 {"name":"file_find","required":["host_id","path","glob"],"properties":{"host_id":"string","path":"string","glob":"string"}},
 {"name":"browser_navigate","required":["host_id","url"],"properties":{"host_id":"string","url":"string","wait_until":"string"}},
 {"name":"browser_elements","required":["host_id"],"properties":{"host_id":"string"}},
 {"name":"browser_click","required":["host_id"],"properties":{"host_id":"string","index":"integer","selector":"string"}},
 {"name":"browser_type","required":["host_id","text"],"properties":{"host_id":"string","text":"string","index":"integer","selector":"string"}},
 {"name":"browser_screenshot","required":["host_id"],"properties":{"host_id":"string","full_page":"boolean"}},
 {"name":"browser_markdown","required":["host_id"],"properties":{"host_id":"string"}},
 {"name":"preview_url","required":["host_id","port"],"properties":{"host_id":"string","port":"integer"}}
]
JSON
  jq -e --slurpfile expected "$expected" '
    def valid_tool($tools; $e):
      any($tools[]; . as $t |
        $t.name == $e.name
        and ($t.inputSchema.type == "object")
        and (($t.inputSchema.required // []) | sort) == ($e.required | sort)
        and (($t.inputSchema.properties // {}) | keys | sort) == ($e.properties | keys | sort)
        and all(($e.properties | to_entries[]); . as $p | $t.inputSchema.properties[$p.key].type == $p.value)
      );
    . as $tools
    | ($tools | length) == 17
      and ([$tools[].name] | sort) == ([$expected[0][] | .name] | sort)
      and all($tools[].name; test("^[a-z][a-z0-9_]*$"))
      and all($expected[0][]; . as $e | valid_tool($tools; $e))
  ' "${VERIFY_TMPDIR}/tools.json" >/dev/null
}

host_payload() {
  jq -e 'if (.host | type) == "object" then .host else . end' "$MCP_PAYLOAD"
}

prepare_lifecycle_host() {
  local args state listed expires_before expires_after elapsed=0 workspace_id="${1:-}" created_workspace_id workspace_body
  if [[ -n "$workspace_id" && ! "$workspace_id" =~ ^w[a-z0-9]{7}$ ]]; then
    LIFECYCLE_OK=0
    return 1
  fi
  if ! portal_request GET /api/hosts || [[ "$API_STATUS" != 200 ]] || ! jq -e --arg name "$OWN_HOST_NAME" '(.items | type) == "array" and ([.items[] | select(.name == $name)] | length == 0)' "$API_JSON" >/dev/null; then
    LIFECYCLE_OK=0
    return 1
  fi
  if [[ -z "$workspace_id" ]]; then
    workspace_body="${VERIFY_TMPDIR}/host-workspace.json"
    jq -cn --arg name "$OWN_HOST_NAME" '{name:$name}' >"$workspace_body"
    portal_request POST /api/workspaces "$workspace_body" "workspace-create-${OWN_HOST_NAME}"
    if [[ "$API_STATUS" != 201 ]] || ! jq -e '.id | type == "string"' "$API_JSON" >/dev/null; then LIFECYCLE_OK=0; return 1; fi
    workspace_id="$(jq -r '.id' "$API_JSON")"
    OWN_WORKSPACE_ID="$workspace_id"
  fi
  args="$(jq -cn --arg template_id "$TEMPLATE" --arg name "$OWN_HOST_NAME" --arg id "$workspace_id" --arg key "host-create-${OWN_HOST_NAME}" '{template_id:$template_id,name:$name,workspace_ids:[$id],idempotency_key:$key}')"
  HOST_CREATE_ATTEMPTED=1
  if ! mcp_tool_call host_create "$args" || ! mcp_result_ok || ! mcp_extract_payload; then LIFECYCLE_OK=0; return 1; fi
  OWN_HOST_ID="$(host_payload | jq -r '.id // empty')"
  OWN_HOST_URL="$(host_payload | jq -r '.url // empty')"
  created_workspace_id="$(host_payload | jq -r '.workspaces[0].workspace.id // empty')"
  if [[ ! "$OWN_HOST_ID" =~ ^h[a-z0-9]{7}$ ]]; then LIFECYCLE_OK=0; OWN_HOST_ID=""; return 1; fi
  if [[ ! "$created_workspace_id" =~ ^w[a-z0-9]{7}$ ]]; then LIFECYCLE_OK=0; return 1; fi
  if [[ -n "$workspace_id" ]]; then
    if [[ "$created_workspace_id" != "$workspace_id" ]]; then LIFECYCLE_OK=0; return 1; fi
  else
    OWN_WORKSPACE_ID="$created_workspace_id"
  fi
  if [[ "$(host_payload | jq -r '.state // empty')" != creating || -z "$OWN_HOST_URL" ]] || ! url_is_local "$OWN_HOST_URL"; then LIFECYCLE_OK=0; fi
  args="$(host_arguments)"

  if ! mcp_tool_call host_list '{}' || ! mcp_result_ok || ! mcp_extract_payload; then LIFECYCLE_OK=0
  else
    listed="$(jq -e --arg id "$OWN_HOST_ID" '((.items // .hosts // []) | any(.id == $id))' "$MCP_PAYLOAD" 2>/dev/null || true)"
    [[ "$listed" == true ]] || LIFECYCLE_OK=0
  fi

  state=""
  while ((elapsed <= MCP_READY_TIMEOUT)); do
    if ! mcp_tool_call host_get "$args" || ! mcp_result_ok || ! mcp_extract_payload; then
      LIFECYCLE_OK=0
      break
    fi
    state="$(host_payload | jq -r '.state // empty')"
    [[ "$state" == running ]] && break
    [[ "$state" != failed ]] || { LIFECYCLE_OK=0; break; }
    ((elapsed == MCP_READY_TIMEOUT)) && break
    sleep "$MCP_POLL_INTERVAL"
    elapsed=$((elapsed + MCP_POLL_INTERVAL))
  done
  [[ "$state" == running ]] || LIFECYCLE_OK=0

  if ! mcp_tool_call host_get "$args" || ! mcp_result_ok || ! mcp_extract_payload; then LIFECYCLE_OK=0; return 1; fi
  expires_before="$(host_payload | jq -r '.expires_at // empty')"
  if ! mcp_tool_call host_renew "$args" || ! mcp_result_ok || ! mcp_extract_payload; then LIFECYCLE_OK=0; return 1; fi
  expires_after="$(host_payload | jq -r '.expires_at // empty')"
  [[ -n "$expires_before" && -n "$expires_after" && "$expires_after" > "$expires_before" ]] || LIFECYCLE_OK=0
}

finish_lifecycle_host() {
  local args
  [[ -n "$OWN_HOST_ID" ]] || { LIFECYCLE_OK=0; return 1; }
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,keep_workspace:false}')"
  if ! mcp_tool_call host_delete "$args" || ! mcp_result_ok || ! mcp_extract_payload; then
    LIFECYCLE_OK=0
    return 1
  fi
  HOST_DELETE_ACCEPTED=1
  [[ "$(host_payload | jq -r '.state // empty')" == deleting ]] || LIFECYCLE_OK=0
}

host_arguments() {
  jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id}'
}

check_shell_exec() {
  local args
  [[ -n "$OWN_HOST_ID" ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,command:"echo hi",cwd:"/home/gem/workspace"}')"
  mcp_tool_call shell_exec "$args" && mcp_result_ok && mcp_extract_payload && \
    jq -e '(.exit_code // .data.exit_code) == 0 and (((.stdout // .output // .data.output // "") | tostring) | contains("hi"))' "$MCP_PAYLOAD" >/dev/null
}

check_file_tools() {
  local path args found text
  [[ -n "$OWN_HOST_ID" ]] || return 1
  path="${WORKSPACE%/}/${FILE_FIXTURE_NAME}"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$path" --arg content "$FILE_FIXTURE_CONTENT" '{host_id:$host_id,path:$path,content:$content}')"
  mcp_tool_call file_write "$args" && mcp_result_ok || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$WORKSPACE" --argjson show_hidden true '{host_id:$host_id,path:$path,show_hidden:$show_hidden}')"
  mcp_tool_call file_list "$args" && mcp_result_ok && mcp_extract_payload || return 1
  found="$(jq -e --arg name "$FILE_FIXTURE_NAME" 'tostring | contains($name)' "$MCP_PAYLOAD" 2>/dev/null || true)"
  [[ "$found" == true ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$WORKSPACE" --arg glob "$FILE_FIXTURE_NAME" '{host_id:$host_id,path:$path,glob:$glob}')"
  mcp_tool_call file_find "$args" && mcp_result_ok && mcp_extract_payload || return 1
  jq -e --arg name "$FILE_FIXTURE_NAME" 'tostring | contains($name)' "$MCP_PAYLOAD" >/dev/null || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$path" '{host_id:$host_id,path:$path}')"
  mcp_tool_call file_read "$args" && mcp_result_ok && mcp_extract_payload || return 1
  text="$(payload_text)"
  [[ "$text" == *"$FILE_FIXTURE_CONTENT"* ]]
}

workspace_is_detached() {
  portal_request GET /api/workspaces || return 1
  [[ "$API_STATUS" == 200 ]] || return 1
  jq -e --arg id "$OWN_WORKSPACE_ID" '
    ([.items[]? | select(.id == $id)] | length) == 1
    and any(.items[]?; .id == $id and .state == "detached" and .attached_host_id == null)
  ' "$API_JSON" >/dev/null
}

wait_for_detached_workspace() {
  local elapsed=0
  while ((elapsed <= WORKSPACE_READY_TIMEOUT)); do
    workspace_is_detached && return 0
    ((elapsed == WORKSPACE_READY_TIMEOUT)) && break
    sleep "$WORKSPACE_POLL_INTERVAL"
    elapsed=$((elapsed + WORKSPACE_POLL_INTERVAL))
  done
  return 1
}

check_workspace_reuse() {
  local args original_host_id
  [[ "$OWN_HOST_ID" =~ ^h[a-z0-9]{7}$ && "$OWN_WORKSPACE_ID" =~ ^w[a-z0-9]{7}$ ]] || { LIFECYCLE_OK=0; return 1; }
  original_host_id="$OWN_HOST_ID"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,keep_workspace:true}')"
  if ! mcp_tool_call host_delete "$args" || ! mcp_result_ok || ! mcp_extract_payload || [[ "$(host_payload | jq -r '.state // empty')" != deleting ]]; then
    LIFECYCLE_OK=0
    return 1
  fi
  HOST_DELETE_ACCEPTED=1
  if ! wait_for_detached_workspace; then
    LIFECYCLE_OK=0
    return 1
  fi

  OWN_HOST_ID=""
  OWN_HOST_URL=""
  OWN_HOST_NAME="verify-ai-reuse-${VERIFY_RESOURCE_SUFFIX}"
  HOST_DELETE_ACCEPTED=0
  if ! prepare_lifecycle_host "$OWN_WORKSPACE_ID"; then
    LIFECYCLE_OK=0
    return 1
  fi

  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "${WORKSPACE%/}/${FILE_FIXTURE_NAME}" '{host_id:$host_id,path:$path}')"
  if ! mcp_tool_call file_read "$args" || ! mcp_result_ok || ! mcp_extract_payload || [[ "$(payload_text)" != *"$FILE_FIXTURE_CONTENT"* ]]; then
    LIFECYCLE_OK=0
    return 1
  fi
  [[ "$OWN_HOST_ID" != "$original_host_id" ]] || LIFECYCLE_OK=0
}

check_browser_read() {
  local args elements text html browser_url
  [[ -n "$OWN_HOST_ID" ]] || return 1
  # Keep browser requests local while exercising the interactive browser tools.
  html="$(cat <<'HTML'
<!doctype html>
<title>Example Domain</title>
<h1>Example Domain</h1>
<button id="verify-button" onclick="document.querySelector('#click-output').textContent='clicked'">Click me</button>
<input id="verify-input" oninput="document.querySelector('#type-output').textContent=this.value">
<p id="click-output">not clicked</p>
<p id="type-output"></p>
HTML
)"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$BROWSER_FIXTURE" --arg content "$html" '{host_id:$host_id,path:$path,content:$content}')"
  mcp_tool_call file_write "$args" && mcp_result_ok || return 1
  browser_url="file://${BROWSER_FIXTURE}"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg url "$browser_url" '{host_id:$host_id,url:$url}')"
  mcp_tool_call browser_navigate "$args" && mcp_result_ok || return 1
  args="$(host_arguments)"
  mcp_tool_call browser_elements "$args" && mcp_result_ok && mcp_extract_payload || return 1
  elements="$(jq -e 'if type == "array" then length > 0 else ((.elements // .items // []) | length > 0) end' "$MCP_PAYLOAD" 2>/dev/null || true)"
  [[ "$elements" == true ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,selector:"#verify-button"}')"
  mcp_tool_call browser_click "$args" && mcp_result_ok || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,selector:"#verify-input",text:"typed locally"}')"
  mcp_tool_call browser_type "$args" && mcp_result_ok || return 1
  args="$(host_arguments)"
  mcp_tool_call browser_markdown "$args" && mcp_result_ok && mcp_extract_payload || return 1
  text="$(payload_text)"
  [[ "$text" == *"Example Domain"* && "$text" == *"clicked"* && "$text" == *"typed locally"* ]]
}

check_screenshot() {
  local args
  [[ -n "$OWN_HOST_ID" ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,full_page:false}')"
  mcp_tool_call browser_screenshot "$args" && mcp_result_ok || return 1
  python3 - "$MCP_JSON" "$MCP_SCREENSHOT_MAX_WIDTH" <<'PY'
import base64
import json
import struct
import sys

response = json.load(open(sys.argv[1], encoding="utf-8"))
limit = int(sys.argv[2])
images = [item for item in response.get("result", {}).get("content", []) if item.get("type") == "image" and item.get("mimeType") == "image/png"]
if not images:
    raise SystemExit(1)
png = base64.b64decode(images[0]["data"], validate=True)
if not png.startswith(b"\x89PNG\r\n\x1a\n") or len(png) < 24:
    raise SystemExit(1)
width, _ = struct.unpack(">II", png[16:24])
raise SystemExit(0 if width <= limit else 1)
PY
}

check_output_truncation() {
  local chars stdout_bytes command args
  [[ -n "$OWN_HOST_ID" ]] || return 1
  chars=$(((MCP_MAX_OUTPUT_BYTES + 2) / 3 + 1024))
  stdout_bytes=$((chars * 3))
  printf -v command "python3 -c 'print(\"界\"*%d,end=\"\")'" "$chars"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg command "$command" --arg cwd "$WORKSPACE" '{host_id:$host_id,command:$command,cwd:$cwd}')"
  mcp_tool_call shell_exec "$args" && mcp_result_ok || return 1
  python3 - "$MCP_JSON" "$stdout_bytes" "$MCP_MAX_OUTPUT_BYTES" <<'PY'
import json
import re
import sys

response = json.load(open(sys.argv[1], encoding="utf-8"))
stdout_bytes = int(sys.argv[2])
max_output_bytes = int(sys.argv[3])
result = response.get("result")
if not isinstance(result, dict) or "structuredContent" in result:
    raise SystemExit("truncated result unexpectedly contains structuredContent")
texts = [item.get("text") for item in result.get("content", []) if item.get("type") == "text"]
if len(texts) != 1 or not isinstance(texts[0], str):
    raise SystemExit("truncated result must contain one text item")
text = texts[0]
match = re.search(r"（已截断，原始 ([0-9]+) 字节）$", text)
if not match:
    raise SystemExit("truncation suffix is missing or not at the end")
original_bytes = int(match.group(1))
if original_bytes <= stdout_bytes:
    raise SystemExit("serialized output length must exceed shell stdout bytes")
suffix = match.group(0)
prefix = text[:match.start()]
prefix_bytes = prefix.encode("utf-8")
suffix_bytes = suffix.encode("utf-8")
if len(prefix_bytes) > max_output_bytes:
    raise SystemExit("UTF-8-cleaned output prefix exceeds the configured limit")
if len(text.encode("utf-8")) != len(prefix_bytes) + len(suffix_bytes):
    raise SystemExit("output contains bytes beyond the truncation prefix and suffix")
PY
}

find_nonexistent_host() {
  local attempt candidate args
  for ((attempt = 0; attempt < 12; attempt++)); do
    printf -v candidate 'h%04x%03x' "$RANDOM" "$RANDOM"
    args="$(jq -cn --arg host_id "$candidate" '{host_id:$host_id}')"
    if mcp_tool_call host_get "$args" && jq -e '(.result.isError == true or has("error")) and (tostring | contains("not_found"))' "$MCP_JSON" >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

check_not_found_conversion() {
  local candidate args
  [[ -n "$OWN_HOST_ID" ]] || return 1
  candidate="$(find_nonexistent_host)" || return 1
  args="$(jq -cn --arg host_id "$candidate" --arg command "true" '{host_id:$host_id,command:$command}')"
  mcp_tool_call shell_exec "$args" || return 1
  jq -e '(.result.isError == true or has("error")) and (tostring | contains("not_found"))' "$MCP_JSON" >/dev/null
}

check_preview_url() {
  local args
  [[ -n "$OWN_HOST_ID" ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" '{host_id:$host_id,port:5173}')"
  mcp_tool_call preview_url "$args" && mcp_result_ok && mcp_extract_payload || return 1
  jq -e --arg expected_host "5173-${OWN_HOST_ID}.${HOST_SUFFIX}" '
    (keys == ["url"])
    and (.url | type == "string" and test("^https?://") and (sub("^https?://"; "") | split("/")[0]) == $expected_host)
  ' "$MCP_PAYLOAD" >/dev/null
}

check_m6_flow() {
  local args markdown path base content
  [[ -n "$OWN_HOST_ID" ]] || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg url "file://${BROWSER_FIXTURE}" '{host_id:$host_id,url:$url}')"
  mcp_tool_call browser_navigate "$args" && mcp_result_ok || return 1
  args="$(host_arguments)"
  mcp_tool_call browser_markdown "$args" && mcp_result_ok && mcp_extract_payload || return 1
  markdown="$(payload_text)"
  [[ "$markdown" == *"Example Domain"* ]] || return 1
  base=".mcp-m6-${$}.md"
  path="${WORKSPACE%/}/${base}"
  content="$markdown"
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$path" --arg content "$content" '{host_id:$host_id,path:$path,content:$content}')"
  mcp_tool_call file_write "$args" && mcp_result_ok || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$WORKSPACE" --argjson show_hidden true '{host_id:$host_id,path:$path,show_hidden:$show_hidden}')"
  mcp_tool_call file_list "$args" && mcp_result_ok && mcp_extract_payload || return 1
  jq -e --arg name "$base" 'tostring | contains($name)' "$MCP_PAYLOAD" >/dev/null || return 1
  args="$(jq -cn --arg host_id "$OWN_HOST_ID" --arg path "$path" '{host_id:$host_id,path:$path}')"
  mcp_tool_call file_read "$args" && mcp_result_ok && mcp_extract_payload || return 1
  [[ "$(payload_text)" == *"Example Domain"* ]]
}

fetch_and_check_audit() {
  portal_request GET "/api/admin/audit?action=mcp.call&since=${RUN_STARTED}&limit=500"
  [[ "$API_STATUS" == 200 ]] || return 1
  cp -- "$API_JSON" "${VERIFY_TMPDIR}/audit.json"
  jq -e --arg host_id "$OWN_HOST_ID" --arg actor_id "$CURRENT_USER_ID" '
    [.items[]? | select(.action == "mcp.call" and ((.target == $host_id) or (.detail.host_id == $host_id)))] as $rows
    | ($rows | length > 0)
      and all($rows[]; .actor_id == $actor_id and .actor_kind == "api_key"
        and ((.detail | keys | sort) == ["duration_ms","host_id","ok","tool"])
        and (.detail.duration_ms | type == "number")
        and (.detail.ok | type == "boolean"))
  ' "$API_JSON" >/dev/null || return 1
}

skip_mcp_checks() {
  local reason="$1" id
  for id in A06.02 A06.03 A06.04 A06.05 A06.06 A06.07 A06.08 A06.09 A06.10 A06.12 A06.13; do
    printf 'SKIP %s %s\n' "$id" "$reason"
  done
}


if [[ "$REAL_RUN" != 0 && "$REAL_RUN" != 1 ]]; then
  fail A06.01 'local MCP integration switch' 'CH_REAL_E2E must be 0 or 1'
  skip_mcp_checks 'invalid CH_REAL_E2E value'
elif [[ "$REAL_RUN" == 1 ]]; then
  if ! validate_real_environment; then
    fail A06.01 'local MCP integration prerequisites' 'set valid CH_* configuration; targets must be loopback or *.localhost and credentials are never printed'
    skip_mcp_checks 'local integration prerequisites are invalid'
  elif ! setup_login_and_key; then
    fail A06.01 'local MCP integration prerequisites' 'admin login or API Key setup failed'
    skip_mcp_checks 'admin login or API Key setup failed'
  else
    check A06.01 'missing and invalid API keys return 401 unauthenticated' test_unauthenticated || true
    if ! initialize_platform; then
      fail A06.01 'valid chk_ API Key initializes the platform MCP session' 'initialize failed'
      skip_mcp_checks 'platform MCP initialize failed'
    elif ! list_all_tools; then
      fail A06.02 'MCP tools/list returns all 17 tools and schemas' 'tools/list failed'
      skip_mcp_checks 'MCP tools/list failed'
    elif ! validate_tool_schemas; then
      fail A06.02 'MCP tools/list returns all 17 tools and schemas' 'tool names, required fields, property names, or schema types differ from the contract'
      skip_mcp_checks 'MCP tool schema validation failed'
    else
      pass A06.02 'MCP tools/list returns all 17 tools with the documented snake_case schemas'
      prepare_lifecycle_host || true
      check A06.04 'shell_exec returns stdout and exit_code 0' check_shell_exec || true
      check A06.05 'file_write, file_list, file_find, and file_read round-trip a private file' check_file_tools || true
      check A06.06 'local browser navigation, elements, click, type, and markdown work' check_browser_read || true
      check A06.07 'browser_screenshot returns a PNG image within the configured width' check_screenshot || true
      check A06.08 'text output is truncated with the original byte count' check_output_truncation || true
      check A06.09 'a random nonexistent host maps to a not_found tool error' check_not_found_conversion || true
      check A06.12 'preview_url returns a private local URL' check_preview_url || true
      check A06.13 'M6 creates a markdown file from the browser page and lists it' check_m6_flow || true
      check_workspace_reuse || true
      finish_lifecycle_host || true
      if ((LIFECYCLE_OK == 1)); then
        pass A06.03 'host lifecycle retains and reattaches its workspace; the original file reads back'
      else
        fail A06.03 'host lifecycle retains and reattaches its workspace; the original file reads back' 'a lifecycle state, workspace attachment, or retained file did not match the contract'
      fi
      check A06.10 'mcp.call audit contains metadata only and no tool parameters or output' fetch_and_check_audit || true
    fi
  fi
else
  printf 'SKIP A06.01 set CH_REAL_E2E=1 with local credentials to run MCP integration checks\n'
  skip_mcp_checks 'set CH_REAL_E2E=1 with local credentials to run MCP integration checks'
fi

printf 'SKIP A06.14 manual VNC observation is not scriptable\n'
printf 'SKIP A06.15 manual login-state continuity check is not scriptable\n'
printf 'SKIP A06.21 private preview and hot-reload interaction requires manual verification\n'
printf 'SKIP A06.22 MCP session beyond 60 seconds requires a long-running client observation\n'

verify_finish
