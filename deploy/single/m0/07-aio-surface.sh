#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-07-aio-surface
m0_require_linux_docker
m0_require_pinned_aio_image

m0_start_aio_container runc

auth_body="$(jq -cn '{command:"printf cloudhost-m0-auth-ok",timeout:10}')"
if m0_request POST "$CH_AIO_URL/v1/shell/exec" "$M0_TMPDIR/response" aio "$auth_body" && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.success == true and .data.status == "completed" and .data.exit_code == 0' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.05 X-AIO-API-Key authorizes shell execution'
else
  m0_fail "A01.05 X-AIO-API-Key probe failed (HTTP $M0_HTTP_CODE)"
fi

if m0_request POST "$CH_AIO_URL/v1/shell/exec" "$M0_TMPDIR/response" none "$auth_body" && [[ "$M0_HTTP_CODE" == 401 ]]; then
  m0_pass 'A01.05 missing AIO API key returns HTTP 401'
else
  m0_fail "A01.05 missing-key probe expected HTTP 401, got $M0_HTTP_CODE"
fi

if m0_request POST "$CH_AIO_URL/v1/shell/exec" "$M0_TMPDIR/response" aio-bearer "$auth_body"; then
  m0_note "Authorization Bearer observation: HTTP $M0_HTTP_CODE"
else
  m0_note 'Authorization Bearer observation: transport failure'
fi
if m0_request POST "$CH_AIO_URL/v1/shell/exec" "$M0_TMPDIR/response" aio-query "$auth_body"; then
  m0_note "api_key query observation: HTTP $M0_HTTP_CODE"
else
  m0_note 'api_key query observation: transport failure'
fi

if m0_request GET "$CH_AIO_URL/v1/shell/terminal-url" "$M0_TMPDIR/response" aio && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.success == true and (.data | type == "string")' "$M0_TMPDIR/response" >/dev/null; then
  terminal_shape="$(jq -r '.data | "type=string chars=\(length) scheme=\(test("^[A-Za-z][A-Za-z0-9+.-]*://")) query=\(contains("?"))"' "$M0_TMPDIR/response")"
  m0_pass "A01.06 terminal URL response recorded ($terminal_shape; value redacted)"
else
  m0_fail "A01.06 terminal URL request failed (HTTP $M0_HTTP_CODE)"
fi

if m0_request GET "$CH_AIO_URL/v1/sandbox" "$M0_TMPDIR/response" aio && [[ "$M0_HTTP_CODE" == 200 ]]; then
  home_dir="$(jq -er '.home_dir' "$M0_TMPDIR/response")" || m0_fatal 'AIO sandbox context omitted home_dir'
  [[ "$home_dir" == /* && "$home_dir" != *$'\n'* ]] || m0_fatal 'AIO returned an invalid home_dir'
else
  m0_fatal "could not read AIO sandbox context (HTTP $M0_HTTP_CODE)"
fi

download_name="cloudhost-m0-$(date +%s)-$$.txt"
download_url='data:text/html,<html><body>cloudhost-m0-download</body></html>'
navigate_body="$(jq -cn --arg url "$download_url" '{url:$url,wait_until:"domcontentloaded",timeout:20}')"
if ! m0_aio_json "$CH_AIO_URL" POST /v1/browser/page/navigate "$navigate_body" || [[ "$M0_HTTP_CODE" != 200 ]]; then
  m0_fail "could not load temporary browser download page (HTTP $M0_HTTP_CODE)"
else
  expression="(() => { const a = document.createElement('a'); a.href = 'data:text/plain,cloudhost-m0-download'; a.download = '$download_name'; document.body.appendChild(a); a.click(); a.remove(); return 'download-triggered'; })()"
  eval_body="$(jq -cn --arg expression "$expression" '{expression:$expression}')"
  if ! m0_aio_json "$CH_AIO_URL" POST /v1/browser/page/evaluate "$eval_body" || [[ "$M0_HTTP_CODE" != 200 ]]; then
    m0_fail "Chromium download trigger failed (HTTP $M0_HTTP_CODE)"
  else
    printf -v quoted_home '%q' "$home_dir"
    download_path=''
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if m0_aio_exec "$CH_AIO_URL" "find $quoted_home -type f -name '$download_name' -print -quit" && [[ "$M0_HTTP_CODE" == 200 ]]; then
        download_path="$(m0_aio_exec_output)"
        [[ -n "$download_path" ]] && break
      fi
      sleep 1
    done
    if [[ -n "$download_path" ]]; then
      m0_pass "A01.06 Chromium test download path: $download_path"
    else
      m0_fail 'A01.06 Chromium download did not appear under the AIO home directory'
    fi
  fi
fi

# The dollar signs must reach awk inside the remote AIO shell.
# shellcheck disable=SC2016
if m0_aio_exec "$CH_AIO_URL" 'df -B1 /dev/shm | awk "END {print \$2 \" bytes total, \" \$4 \" bytes available\"}"' && [[ "$M0_HTTP_CODE" == 200 ]]; then
  shm_report="$(m0_aio_exec_output)"
  m0_note "/dev/shm: $shm_report"
  if [[ "$shm_report" =~ ^[0-9]+\ bytes\ total,\ [0-9]+\ bytes\ available$ ]]; then
    m0_pass 'A01.06 AIO /dev/shm size recorded'
  else
    m0_fail 'A01.06 could not parse AIO /dev/shm size'
  fi
else
  m0_fail "A01.06 could not inspect AIO /dev/shm (HTTP $M0_HTTP_CODE)"
fi

m0_pass 'A01.05/A01.06 AIO auth, terminal URL, browser download and shm checks completed'
m0_finish
