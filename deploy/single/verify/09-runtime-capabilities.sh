#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/../m0/lib.sh"
m0_begin t09-runtime-capabilities
m0_require curl jq python3
m0_require_osb
m0_require_pinned_aio_image

python3 - "$CH_OSB_URL" <<'PY' || m0_fatal 'T09 probes require a loopback OpenSandbox URL'
import sys
from urllib.parse import urlparse

url = urlparse(sys.argv[1])
if url.scheme not in ("http", "https") or url.hostname not in ("127.0.0.1", "localhost", "::1"):
    raise SystemExit(1)
PY

M0_RUN_ID="t09-$(date -u +%Y%m%dT%H%M%SZ)-$$"
m0_generate_aio_key

t09_aio_container_ids() {
  if [[ -n "${CH_T09_LIMA_HOME:-}" && -n "${CH_T09_LIMACTL:-}" && -n "${CH_T09_LIMA_INSTANCE:-}" ]]; then
    LIMA_HOME="$CH_T09_LIMA_HOME" "$CH_T09_LIMACTL" shell "$CH_T09_LIMA_INSTANCE" \
      sudo -n docker ps -q --filter "ancestor=$CH_AIO_IMAGE"
  elif [[ "$(uname -s)" == Linux ]] && command -v docker >/dev/null 2>&1; then
    docker ps -q --filter "ancestor=$CH_AIO_IMAGE"
  else
    return 2
  fi
}

t09_inspect_aio_container() {
  local id="$1"
  if [[ -n "${CH_T09_LIMA_HOME:-}" && -n "${CH_T09_LIMACTL:-}" && -n "${CH_T09_LIMA_INSTANCE:-}" ]]; then
    LIMA_HOME="$CH_T09_LIMA_HOME" "$CH_T09_LIMACTL" shell "$CH_T09_LIMA_INSTANCE" \
      sudo -n docker inspect --format 'id={{.Id}} image_id={{.Image}} configured_image={{.Config.Image}} runtime={{.HostConfig.Runtime}} status={{.State.Status}}' "$id"
  elif [[ "$(uname -s)" == Linux ]] && command -v docker >/dev/null 2>&1; then
    docker inspect --format 'id={{.Id}} image_id={{.Image}} configured_image={{.Config.Image}} runtime={{.HostConfig.Runtime}} status={{.State.Status}}' "$id"
  else
    return 2
  fi
}

T09_AIO_CONTAINER_IDS_BEFORE=""
if T09_AIO_CONTAINER_IDS_BEFORE="$(t09_aio_container_ids 2>/dev/null)"; then
  count_before="$(printf '%s\n' "$T09_AIO_CONTAINER_IDS_BEFORE" | sed '/^$/d' | wc -l | tr -d ' ')"
  m0_note "T09 local Docker runtime sees $count_before pinned AIO container(s) before create"
else
  m0_note 'T09 Docker container inspection is unavailable from this probe client'
fi

if ! m0_osb_json GET /sandboxes || [[ "$M0_HTTP_CODE" != 200 ]] || ! jq -e '.items | type == "array"' "$M0_TMPDIR/response" >/dev/null; then
  m0_fatal "local OpenSandbox inventory failed (HTTP $M0_HTTP_CODE)"
fi
m0_note "T09 isolated OpenSandbox is reachable; existing sandbox count=$(jq '.items | length' "$M0_TMPDIR/response")"

sandbox_body="$(m0_create_aio_sandbox_body)"
if ! m0_osb_create "$sandbox_body"; then
  m0_fatal "could not create isolated fixed-image sandbox (HTTP $M0_HTTP_CODE)"
fi
sandbox_id="$M0_LAST_SANDBOX_ID"
m0_note "T09 sandbox_id=$sandbox_id image=$CH_AIO_IMAGE run_id=$M0_RUN_ID"

if ! m0_osb_wait_state "$sandbox_id" Running "$CH_OSB_READY_TIMEOUT"; then
  m0_fatal "T09 sandbox did not reach Running (state=$(m0_osb_state "$sandbox_id"))"
fi
m0_pass 'T09 fixed-image OpenSandbox reached Running'

if ! m0_osb_endpoint "$sandbox_id" 8080; then
  m0_fatal "T09 AIO endpoint lookup failed (HTTP $M0_HTTP_CODE)"
fi
aio_url="$M0_LAST_ENDPOINT_URL"
endpoint_authority="${aio_url#*://}"
endpoint_authority="${endpoint_authority%%/*}"
m0_note "T09 AIO endpoint authority=$endpoint_authority (credentials redacted)"
m0_aio_wait_ready "$aio_url" || m0_fatal 'T09 AIO shell API did not become ready'

if m0_request GET "$aio_url/v1/sandbox" "$M0_TMPDIR/sandbox-context" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
  jq -c '{home_dir, sandbox_id, id, arch, architecture, os}' "$M0_TMPDIR/sandbox-context"
else
  m0_note "T09 AIO sandbox-context endpoint unavailable (HTTP $M0_HTTP_CODE)"
fi

if m0_request GET "$aio_url/v1/docs" "$M0_TMPDIR/docs" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
  m0_pass 'T09 authenticated AIO docs endpoint responds'
else
  m0_fail "T09 authenticated AIO docs endpoint returned HTTP $M0_HTTP_CODE"
fi

if m0_request GET "$aio_url/v1/openapi.json" "$M0_TMPDIR/openapi" aio && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.paths | type == "object"' "$M0_TMPDIR/openapi" >/dev/null; then
  jq -r '
    .paths | to_entries[] as $path
    | $path.value | keys[] as $method
    | select(["get","post","put","patch","delete","options","head"] | index($method))
    | select($path.key | test("/(browser|window|file|shell|terminal|vnc|sandbox|mcp)"))
    | "T09 route \($method|ascii_upcase) \($path.key)"
  ' "$M0_TMPDIR/openapi"
  m0_pass 'T09 actual fixed-image OpenAPI route inventory captured'
else
  m0_note "T09 /v1/openapi.json unavailable (HTTP $M0_HTTP_CODE); /v1/docs is the available documentation surface"
fi

mcp_request='{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
if m0_request POST "$aio_url/mcp" "$M0_TMPDIR/mcp-raw" aio-mcp "$mcp_request" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  if jq -e . "$M0_TMPDIR/mcp-raw" >/dev/null 2>&1; then
    cp "$M0_TMPDIR/mcp-raw" "$M0_TMPDIR/mcp.json"
  elif ! sed -n 's/^data: *//p' "$M0_TMPDIR/mcp-raw" | tail -n 1 >"$M0_TMPDIR/mcp.json" || ! jq -e . "$M0_TMPDIR/mcp.json" >/dev/null 2>&1; then
    m0_fatal 'T09 MCP tools/list returned an unreadable JSON/SSE response'
  fi
  jq -e '.jsonrpc == "2.0" and (.result.tools | type == "array")' "$M0_TMPDIR/mcp.json" >/dev/null || m0_fatal 'T09 MCP tools/list schema is invalid'
  mcp_tools="$(jq -r '[.result.tools[].name] | join(", ")' "$M0_TMPDIR/mcp.json")"
  m0_note "T09 AIO MCP tools ($(( $(jq '.result.tools | length' "$M0_TMPDIR/mcp.json") ))): $mcp_tools"
  jq -r '.result.tools[] | select(.name | test("window|tab"; "i")) | "T09 window/tab MCP tool: \(.name)"' "$M0_TMPDIR/mcp.json"
  jq -c '.result.tools[] | select(.name | test("browser_gui|browser_.*tab")) | {name,description,inputSchema}' "$M0_TMPDIR/mcp.json" | while IFS= read -r tool; do m0_note "T09 MCP tool contract: $tool"; done
else
  m0_fail "T09 authenticated AIO MCP tools/list returned HTTP $M0_HTTP_CODE"
fi

if m0_request POST "$aio_url/v1/shell/exec" "$M0_TMPDIR/response" none \
    "$(jq -cn '{command:"printf t09-auth-denied",timeout:5}')" && [[ "$M0_HTTP_CODE" == 401 ]]; then
  m0_pass 'T09 unauthenticated AIO shell request is rejected with HTTP 401'
else
  m0_fail "T09 unauthenticated AIO shell request expected 401, got HTTP $M0_HTTP_CODE"
fi

gui_auth_body='{"action_type":"MOVE_TO","x":1,"y":1}'
if m0_request POST "$aio_url/v1/browser/actions" "$M0_TMPDIR/response" none "$gui_auth_body" && [[ "$M0_HTTP_CODE" == 401 ]]; then
  m0_pass 'T09 unauthenticated direct GUI input is rejected with HTTP 401'
else
  m0_fail "T09 unauthenticated direct GUI input expected 401, got HTTP $M0_HTTP_CODE"
fi
if m0_aio_json "$aio_url" POST /v1/browser/actions "$gui_auth_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'T09 the shared AIO key can directly send GUI input'
else
  m0_fail "T09 authenticated direct GUI input failed (HTTP $M0_HTTP_CODE)"
fi

boundary_file="/tmp/.t09-auth-${M0_RUN_ID}"
boundary_write="$(jq -cn --arg file "$boundary_file" '{file:$file,content:"synthetic",append:false}')"
if m0_request POST "$aio_url/v1/file/write" "$M0_TMPDIR/response" none "$boundary_write" && [[ "$M0_HTTP_CODE" == 401 ]]; then
  m0_pass 'T09 unauthenticated direct file write is rejected with HTTP 401'
else
  m0_fail "T09 unauthenticated direct file write expected 401, got HTTP $M0_HTTP_CODE"
fi
if m0_aio_json "$aio_url" POST /v1/file/write "$boundary_write" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'T09 the shared AIO key can directly write files'
else
  m0_fail "T09 authenticated direct file write failed (HTTP $M0_HTTP_CODE)"
fi
boundary_cleanup="$(jq -cn --arg command "rm -f -- '$boundary_file'" '{command:$command,timeout:5,hard_timeout:10,truncate:false}')"
m0_aio_json "$aio_url" POST /v1/shell/exec "$boundary_cleanup" >/dev/null || m0_note 'T09 boundary fixture cleanup is guaranteed by disposable sandbox deletion'

shell_body="$(jq -cn --arg command "printf '%s' t09-shell-ok" '{command:$command,timeout:10}')"
if m0_aio_json "$aio_url" POST /v1/shell/exec "$shell_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true and .data.exit_code == 0 and (.data.output | contains("t09-shell-ok"))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'T09 authenticated shell execution returned the synthetic marker'
else
  m0_fail "T09 authenticated shell probe failed (HTTP $M0_HTTP_CODE)"
fi

fixture="/home/gem/workspace/.t09-${M0_RUN_ID}.txt"
fixture_parent="${fixture%/*}"
fixture_body="$(jq -cn --arg file "$fixture" --arg content "t09-file-${M0_RUN_ID}" '{file:$file,content:$content,append:false}')"
if m0_aio_json "$aio_url" POST /v1/file/write "$fixture_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
  read_body="$(jq -cn --arg file "$fixture" '{file:$file}')"
  if m0_aio_json "$aio_url" POST /v1/file/read "$read_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e --arg marker "t09-file-${M0_RUN_ID}" 'tostring | contains($marker)' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 file write/read round trip succeeded'
  else
    m0_fail "T09 file read failed (HTTP $M0_HTTP_CODE)"
  fi
  list_body="$(jq -cn --arg path "$fixture_parent" '{path:$path,recursive:false,show_hidden:true,include_size:true}')"
  if m0_aio_json "$aio_url" POST /v1/file/list "$list_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e --arg file "${fixture##*/}" 'tostring | contains($file)' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 file list found the synthetic fixture'
  else
    m0_fail "T09 file list failed to find the fixture (HTTP $M0_HTTP_CODE)"
  fi
  find_body="$(jq -cn --arg path "$fixture_parent" --arg glob "${fixture##*/}" '{path:$path,glob:$glob}')"
  if m0_aio_json "$aio_url" POST /v1/file/find "$find_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e --arg file "${fixture##*/}" 'tostring | contains($file)' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 file find found the synthetic fixture'
  else
    m0_fail "T09 file find failed to find the fixture (HTTP $M0_HTTP_CODE)"
  fi
else
  m0_fail "T09 file write failed (HTTP $M0_HTTP_CODE)"
fi

page="data:text/html;charset=utf-8,<main id='t09-marker'>t09-browser-ok</main>"
navigate_body="$(jq -cn --arg url "$page" '{url:$url,wait_until:"domcontentloaded",timeout:20}')"
if m0_aio_json "$aio_url" POST /v1/browser/page/navigate "$navigate_body" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  eval_body='{"expression":"document.querySelector(\"#t09-marker\")?.textContent"}'
  if m0_aio_json "$aio_url" POST /v1/browser/page/evaluate "$eval_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e '.success == true and .data == "t09-browser-ok"' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 browser navigation and structured DOM evaluation succeeded'
  else
    m0_fail "T09 browser DOM evaluation failed (HTTP $M0_HTTP_CODE)"
  fi
else
  m0_fail "T09 browser navigation failed (HTTP $M0_HTTP_CODE)"
fi

if m0_request GET "$aio_url/v1/browser/page/elements" "$M0_TMPDIR/elements" aio && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e . "$M0_TMPDIR/elements" >/dev/null; then
  m0_pass 'T09 structured browser elements endpoint returned JSON'
else
  m0_note "T09 structured browser elements endpoint unavailable (HTTP $M0_HTTP_CODE)"
fi

gui_screenshot_request='{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"browser_gui_screenshot","arguments":{}}}'
if m0_request POST "$aio_url/mcp" "$M0_TMPDIR/gui-screenshot-raw" aio-mcp "$gui_screenshot_request" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  if jq -e . "$M0_TMPDIR/gui-screenshot-raw" >/dev/null 2>&1; then
    cp "$M0_TMPDIR/gui-screenshot-raw" "$M0_TMPDIR/gui-screenshot.json"
  else
    sed -n 's/^data: *//p' "$M0_TMPDIR/gui-screenshot-raw" | tail -n 1 >"$M0_TMPDIR/gui-screenshot.json"
  fi
  if jq -e '.result.isError != true and (.result.content | type == "array")' "$M0_TMPDIR/gui-screenshot.json" >/dev/null; then
    image_count="$(jq '[.result.content[]? | select(.type == "image")] | length' "$M0_TMPDIR/gui-screenshot.json")"
    image_bytes="$(jq '[.result.content[]? | select(.type == "image") | (.data // "") | length] | add // 0' "$M0_TMPDIR/gui-screenshot.json")"
    m0_note "T09 GUI MCP screenshot succeeded; image_count=$image_count encoded_bytes=$image_bytes; image data remains private"
  else
    m0_note 'T09 GUI MCP screenshot returned no image result'
  fi
else
  m0_note "T09 GUI MCP screenshot tool unavailable (HTTP $M0_HTTP_CODE)"
fi

for screenshot_path in /v1/browser/page/screenshot /v1/browser/screenshot; do
  screenshot_file="$M0_TMPDIR/screenshot-$(basename "$screenshot_path").png"
  if m0_request GET "$aio_url$screenshot_path" "$screenshot_file" aio && [[ "$M0_HTTP_CODE" == 200 ]] \
      && [[ "$(od -An -tx1 -N8 "$screenshot_file" | tr -d ' \n')" == 89504e470d0a1a0a ]]; then
    dimensions="$(python3 - "$screenshot_file" <<'PY'
import struct, sys
with open(sys.argv[1], 'rb') as image:
    header = image.read(24)
if len(header) != 24 or header[12:16] != b'IHDR':
    raise SystemExit(1)
print(f'{struct.unpack(">II", header[16:24])[0]}x{struct.unpack(">II", header[16:24])[1]}')
PY
)"
    m0_note "T09 screenshot $screenshot_path PNG $dimensions; image remains in private temporary directory"
  else
    m0_note "T09 screenshot endpoint $screenshot_path unavailable or not PNG (HTTP $M0_HTTP_CODE)"
  fi
done

for gui_path in '/vnc/index.html?autoconnect=true' /v1/shell/terminal-url; do
  if m0_request GET "$aio_url$gui_path" "$M0_TMPDIR/gui-response" none && [[ "$M0_HTTP_CODE" == 401 ]]; then
    m0_pass "T09 keyless GUI/terminal surface $gui_path is rejected with HTTP 401"
  else
    m0_fail "T09 keyless GUI/terminal surface $gui_path expected 401 (HTTP $M0_HTTP_CODE)"
  fi
  if m0_request GET "$aio_url$gui_path" "$M0_TMPDIR/gui-response" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
    m0_pass "T09 AIO-key GUI/terminal surface $gui_path returned HTTP 200"
  else
    m0_note "T09 authenticated GUI/terminal surface $gui_path unavailable (HTTP $M0_HTTP_CODE)"
  fi
done

if m0_request GET "$aio_url/v1/browser/tabs" "$M0_TMPDIR/tabs" aio && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e . "$M0_TMPDIR/tabs" >/dev/null; then
  m0_pass 'T09 browser tab list endpoint returns JSON'
  jq -c '{success, tab_count: ((.data // .tabs // []) | if type == "array" then length else null end)}' "$M0_TMPDIR/tabs"
else
  m0_note "T09 browser tab list endpoint unavailable (HTTP $M0_HTTP_CODE)"
fi

rm_body="$(jq -cn --arg command "rm -f -- '$fixture'" '{command:$command,timeout:10}')"
if m0_aio_json "$aio_url" POST /v1/shell/exec "$rm_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true and .data.exit_code == 0' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'T09 synthetic file and browser state cleanup completed'
else
  m0_note 'T09 fixture cleanup will also be guaranteed by disposable sandbox deletion'
fi

# AIO's session kill is tested against an explicitly detached synthetic process.
probe_session_id="t09-stop-${M0_RUN_ID}"
detached_pidfile="/tmp/.${probe_session_id}.pid"
detached_inner_pidfile="/tmp/.${probe_session_id}.inner-pid"
detached_sleep_pidfile="/tmp/.${probe_session_id}.sleep-pid"
detached_marker="/tmp/.${probe_session_id}.done"
session_body="$(jq -cn --arg id "$probe_session_id" --arg dir /tmp '{id:$id,exec_dir:$dir}')"
if m0_aio_json "$aio_url" POST /v1/shell/sessions/create "$session_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
  detached_command="nohup sh -c 'echo \$\$ > \"$detached_inner_pidfile\"; sleep 60 & echo \$! > \"$detached_sleep_pidfile\"; wait; echo done > \"$detached_marker\"' </dev/null >/dev/null 2>&1 & echo \$! > \"$detached_pidfile\"; printf launched"
  detached_body="$(jq -cn --arg id "$probe_session_id" --arg command "$detached_command" '{id:$id,command:$command,timeout:10,hard_timeout:20,truncate:false}')"
  if m0_aio_json "$aio_url" POST /v1/shell/exec "$detached_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e '.success == true and (.data.output | contains("launched"))' "$M0_TMPDIR/response" >/dev/null; then
    kill_session_body="$(jq -cn --arg id "$probe_session_id" '{id:$id}')"
    if m0_aio_json "$aio_url" POST /v1/shell/kill "$kill_session_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
        && jq -e '.success == true' "$M0_TMPDIR/response" >/dev/null; then
      check_detached_command="alive=0; for f in '$detached_pidfile' '$detached_inner_pidfile' '$detached_sleep_pidfile'; do if [ -f \"\$f\" ]; then p=\$(cat \"\$f\" 2>/dev/null || true); case \"\$p\" in ''|*[!0-9]*) continue;; esac; if kill -0 \"\$p\" 2>/dev/null; then alive=1; fi; fi; done; if [ \"\$alive\" -eq 1 ]; then echo CHILD_ALIVE; else echo CHILD_STOPPED; fi"
      check_detached_body="$(jq -cn --arg command "$check_detached_command" '{command:$command,timeout:10,hard_timeout:15,truncate:false}')"
      if m0_aio_json "$aio_url" POST /v1/shell/exec "$check_detached_body" && [[ "$M0_HTTP_CODE" == 200 ]]; then
        detached_state="$(jq -r '.data.output // empty' "$M0_TMPDIR/response")"
        if [[ "$detached_state" == *CHILD_ALIVE* ]]; then
          m0_note 'T09 capability gap: /v1/shell/kill ended its session but a detached child process survived'
        elif [[ "$detached_state" == *CHILD_STOPPED* ]]; then
          m0_pass 'T09 shell session kill also stopped the detached child process'
        else
          m0_fail 'T09 could not determine whether the detached child survived session kill'
        fi
      else
        m0_fail "T09 detached-process check failed (HTTP $M0_HTTP_CODE)"
      fi
    else
      m0_fail "T09 shell session kill probe failed (HTTP $M0_HTTP_CODE)"
    fi
  else
    m0_fail "T09 detached-process launch failed (HTTP $M0_HTTP_CODE)"
  fi
  cleanup_detached_command="for f in '$detached_pidfile' '$detached_inner_pidfile' '$detached_sleep_pidfile'; do if [ -f \"\$f\" ]; then p=\$(cat \"\$f\" 2>/dev/null || true); case \"\$p\" in ''|*[!0-9]*) continue;; esac; kill -TERM \"\$p\" 2>/dev/null || true; fi; done; sleep 1; for f in '$detached_pidfile' '$detached_inner_pidfile' '$detached_sleep_pidfile'; do if [ -f \"\$f\" ]; then p=\$(cat \"\$f\" 2>/dev/null || true); case \"\$p\" in ''|*[!0-9]*) continue;; esac; kill -KILL \"\$p\" 2>/dev/null || true; fi; done; rm -f -- '$detached_pidfile' '$detached_inner_pidfile' '$detached_sleep_pidfile' '$detached_marker'; echo T09_CHILD_CLEANED"
  cleanup_detached_body="$(jq -cn --arg command "$cleanup_detached_command" '{command:$command,timeout:5,hard_timeout:10,truncate:false}')"
  if m0_aio_json "$aio_url" POST /v1/shell/exec "$cleanup_detached_body" && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e '.success == true and (.data.output | contains("T09_CHILD_CLEANED"))' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 detached-process fixture cleanup completed'
  else
    m0_fail "T09 detached-process fixture cleanup failed (HTTP $M0_HTTP_CODE)"
  fi
else
  m0_fail "T09 shell session creation for stop probe failed (HTTP $M0_HTTP_CODE)"
fi

post_kill_screenshot_request='{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"browser_gui_screenshot","arguments":{}}}'
if m0_request POST "$aio_url/mcp" "$M0_TMPDIR/post-kill-screenshot-raw" aio-mcp "$post_kill_screenshot_request" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  if jq -e . "$M0_TMPDIR/post-kill-screenshot-raw" >/dev/null 2>&1; then
    cp "$M0_TMPDIR/post-kill-screenshot-raw" "$M0_TMPDIR/post-kill-screenshot.json"
  else
    sed -n 's/^data: *//p' "$M0_TMPDIR/post-kill-screenshot-raw" | tail -n 1 >"$M0_TMPDIR/post-kill-screenshot.json"
  fi
  if jq -e '.result.isError != true and ([.result.content[]? | select(.type == "image")] | length > 0)' "$M0_TMPDIR/post-kill-screenshot.json" >/dev/null; then
    m0_pass 'T09 GUI screenshot remains available after shell-session kill'
  else
    m0_note 'T09 capability gap: GUI screenshot was unavailable after shell-session kill'
  fi
else
  m0_note "T09 GUI screenshot-after-kill probe unavailable (HTTP $M0_HTTP_CODE)"
fi

hard_timeout_id="t09-hard-${M0_RUN_ID}"
hard_timeout_session="$(jq -cn --arg id "$hard_timeout_id" --arg dir /tmp '{id:$id,exec_dir:$dir}')"
if m0_aio_json "$aio_url" POST /v1/shell/sessions/create "$hard_timeout_session" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  hard_timeout_command="$(jq -cn --arg id "$hard_timeout_id" '{id:$id,command:"sleep 60",timeout:1,hard_timeout:2,truncate:false}')"
  if m0_aio_json "$aio_url" POST /v1/shell/exec "$hard_timeout_command" && [[ "$M0_HTTP_CODE" == 200 ]]; then
    wait_body="$(jq -cn --arg id "$hard_timeout_id" '{id:$id,max_wait_seconds:5}')"
    if m0_aio_json "$aio_url" POST /v1/shell/wait "$wait_body" && [[ "$M0_HTTP_CODE" == 200 ]]; then
      hard_timeout_status="$(jq -r '.data.status // empty' "$M0_TMPDIR/response")"
      if [[ "$hard_timeout_status" == hard_timeout ]]; then
        m0_pass 'T09 hard_timeout terminated the long-running foreground command'
      else
        m0_note "T09 capability gap: foreground command ended with status=${hard_timeout_status:-unknown} after hard_timeout"
      fi
    else
      m0_fail "T09 hard_timeout wait probe failed (HTTP $M0_HTTP_CODE)"
    fi
  else
    m0_fail "T09 hard_timeout command failed to start (HTTP $M0_HTTP_CODE)"
  fi
  kill_hard_timeout="$(jq -cn --arg id "$hard_timeout_id" '{id:$id}')"
  m0_aio_json "$aio_url" POST /v1/shell/kill "$kill_hard_timeout" >/dev/null || true
else
  m0_fail "T09 shell session creation for hard_timeout probe failed (HTTP $M0_HTTP_CODE)"
fi

record_path="/tmp/.t09-${M0_RUN_ID}.mp4"
record_start="$(jq -cn --arg path "$record_path" '{action:"start",save_path:$path,duration:20,fps:1,quality:20}')"
if m0_aio_json "$aio_url" POST /v1/browser/page/record "$record_start" && [[ "$M0_HTTP_CODE" == 200 ]] \
    && jq -e '.success == true and .data.is_recording == true' "$M0_TMPDIR/response" >/dev/null; then
  if m0_aio_json "$aio_url" POST /v1/browser/page/record '{"action":"stop"}' && [[ "$M0_HTTP_CODE" == 200 ]] \
      && jq -e '.success == true and .data.status == "stopped"' "$M0_TMPDIR/response" >/dev/null; then
    m0_pass 'T09 continuous browser recording stopped through the AIO endpoint'
  else
    m0_note "T09 capability gap: browser recording did not confirm stop (HTTP $M0_HTTP_CODE)"
  fi
else
  m0_note "T09 browser recording endpoint unavailable (HTTP $M0_HTTP_CODE)"
fi
remove_record="$(jq -cn --arg command "rm -f -- '$record_path'" '{command:$command,timeout:5,hard_timeout:10,truncate:false}')"
m0_aio_json "$aio_url" POST /v1/shell/exec "$remove_record" >/dev/null || true
m0_note 'T09 browser_gui_execute_action exposes no operation ID or per-action cancellation endpoint; queued/in-flight GUI cancellation remains unproven'

repl_marker="/tmp/.t09-${M0_RUN_ID}.repl-done"
repl_code="import time; time.sleep(3); open('$repl_marker', 'w').write('done')"
repl_request="$(jq -cn --arg code "$repl_code" '{jsonrpc:"2.0",id:3,method:"tools/call",params:{name:"sandbox_execute_code",arguments:{code:$code,language:"python",timeout:1}}}')"
if m0_request POST "$aio_url/mcp" "$M0_TMPDIR/repl-raw" aio-mcp "$repl_request" && [[ "$M0_HTTP_CODE" == 200 ]]; then
  sleep 4
  check_repl_command="if [ -f '$repl_marker' ]; then echo REPL_FINISHED_AFTER_TIMEOUT; else echo REPL_NOT_FINISHED; fi; rm -f -- '$repl_marker'"
  check_repl_body="$(jq -cn --arg command "$check_repl_command" '{command:$command,timeout:5,hard_timeout:10,truncate:false}')"
  if m0_aio_json "$aio_url" POST /v1/shell/exec "$check_repl_body" && [[ "$M0_HTTP_CODE" == 200 ]]; then
    repl_state="$(jq -r '.data.output // empty' "$M0_TMPDIR/response")"
    if [[ "$repl_state" == *REPL_FINISHED_AFTER_TIMEOUT* ]]; then
      m0_note 'T09 capability gap: sandbox_execute_code continued and wrote its synthetic marker after timeout=1 returned'
    elif [[ "$repl_state" == *REPL_NOT_FINISHED* ]]; then
      m0_pass 'T09 sandbox_execute_code did not write its marker after the one-second timeout'
    else
      m0_fail 'T09 could not determine whether the timed-out REPL continued'
    fi
  else
    m0_fail "T09 REPL continuation check failed (HTTP $M0_HTTP_CODE)"
  fi
else
  m0_note "T09 sandbox_execute_code timeout probe unavailable (HTTP $M0_HTTP_CODE)"
fi

m0_note "T09 instance identity: OpenSandbox sandbox_id=$sandbox_id; Morde Host ID is not assigned by this direct probe"
if T09_AIO_CONTAINER_IDS_AFTER="$(t09_aio_container_ids 2>/dev/null)"; then
  T09_NEW_AIO_CONTAINER_IDS="$(printf '%s\n' "$T09_AIO_CONTAINER_IDS_AFTER" | sed '/^$/d' | while IFS= read -r id; do
    if ! printf '%s\n' "$T09_AIO_CONTAINER_IDS_BEFORE" | grep -Fqx "$id"; then printf '%s\n' "$id"; fi
  done)"
  T09_NEW_AIO_CONTAINER_COUNT="$(printf '%s\n' "$T09_NEW_AIO_CONTAINER_IDS" | sed '/^$/d' | wc -l | tr -d ' ')"
  if [[ "$T09_NEW_AIO_CONTAINER_COUNT" == 1 ]]; then
    T09_NEW_AIO_CONTAINER_ID="$(printf '%s\n' "$T09_NEW_AIO_CONTAINER_IDS" | sed -n '1p')"
    if ! t09_inspect_aio_container "$T09_NEW_AIO_CONTAINER_ID" | sed 's/^/INFO T09 runtime inspection /'; then
      m0_note 'T09 could not inspect the isolated container runtime'
    fi
  else
    m0_note "T09 could not uniquely identify its new container (new matching containers=$T09_NEW_AIO_CONTAINER_COUNT)"
  fi
else
  m0_note 'T09 runtime inspection unavailable from this probe client'
fi

if [[ "${CH_T09_KEEP_SANDBOX:-0}" == 1 && "$M0_FAILURES" == 0 ]]; then
  session_file="${CH_T09_SESSION_FILE:-.local/t09-browser-session.json}"
  [[ "$session_file" == .local/* ]] || m0_fatal 'T09 session files must remain under .local/'
  [[ ! -e "$session_file" ]] || m0_fatal 'T09 session file already exists; refusing to overwrite it'
  mkdir -p -- "$(dirname -- "$session_file")"
  session_tmp="$session_file.tmp.$$"
  (umask 077; jq -cn --arg sandbox_id "$sandbox_id" --arg aio_url "$aio_url" --arg api_key "$M0_AIO_KEY" \
    '{sandbox_id:$sandbox_id,aio_url:$aio_url,api_key:$api_key}' >"$session_tmp")
  chmod 600 "$session_tmp"
  mv -- "$session_tmp" "$session_file"
  M0_SANDBOX_IDS=()
  m0_note "T09 isolated sandbox retained for browser verification; private session file=$session_file"
fi

m0_finish
