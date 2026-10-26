#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-04-volume
m0_require_osb_and_linux
m0_require_pinned_aio_image
m0_generate_aio_key

M0_RUN_ID="m0-04-$(date +%s)-$$"
volume_name="cloudhost-m0-${M0_RUN_ID}"
assert_volume_request_body() {
  local body
  body="$(m0_create_aio_sandbox_body "$volume_name")"
  jq -e --arg volume "$volume_name" --arg size "${CH_TEST_VOLUME_SIZE:-20Gi}" '
    (has("networkPolicy") | not)
    and (.volumes | length == 1)
    and any(.volumes[]; .name == "workspace" and .pvc.claimName == $volume
      and .pvc.storage == $size and .mountPath == "/home/gem/workspace")
  ' <<<"$body" >/dev/null || m0_fatal 'volume sandbox request body is invalid'
}
assert_volume_request_body
docker volume inspect "$volume_name" >/dev/null 2>&1 && m0_fatal "test volume already exists: $volume_name"
docker volume create "$volume_name" >/dev/null || m0_fatal 'could not create isolated named volume'
M0_CREATED_VOLUMES+=("$volume_name")
docker image inspect "$CH_AIO_IMAGE" >/dev/null 2>&1 || docker pull "$CH_AIO_IMAGE" >/dev/null || m0_fatal 'could not pull configured AIO image'

marker="cloudhost-volume-${M0_RUN_ID}"
marker_file="/home/gem/workspace/${M0_RUN_ID}.txt"
printf -v quoted_marker_file '%q' "$marker_file"
printf -v quoted_marker '%q' "$marker"

create_volume_sandbox() {
  m0_capture_aio_container_ids
  local sandbox_body
  sandbox_body="$(m0_create_aio_sandbox_body "$volume_name")"
  m0_osb_create "$sandbox_body" || return 1
  m0_osb_wait_state "$M0_LAST_SANDBOX_ID" Running "$CH_OSB_READY_TIMEOUT" || return 1
  m0_osb_endpoint "$M0_LAST_SANDBOX_ID" 8080 || return 1
  m0_aio_wait_ready "$M0_LAST_ENDPOINT_URL" || return 1
  m0_check_created_sandbox_container
}

if ! create_volume_sandbox; then
  m0_fail "A01.08 first volume-backed sandbox failed (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
first_sandbox_id="$M0_LAST_SANDBOX_ID"
first_endpoint_url="$M0_LAST_ENDPOINT_URL"
if m0_aio_exec "$first_endpoint_url" "test \"\$(id -un)\" = gem && test -w /home/gem/workspace && printf '%s\\n' $quoted_marker > $quoted_marker_file && cat $quoted_marker_file" \
  && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e --arg marker "$marker" '.data.exit_code == 0 and ((.data.output // "") | contains($marker))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.08 gem can write the mounted workspace volume'
else
  m0_fail "A01.08 first sandbox could not write its workspace volume (HTTP $M0_HTTP_CODE)"
  m0_finish
fi

if m0_osb_delete "$first_sandbox_id"; then
  m0_pass 'A01.08 first sandbox deleted before volume reuse'
else
  m0_fail 'A01.08 first sandbox could not be deleted'
  m0_finish
fi

attempt=0
while ((attempt < 30)); do
  attached="$(docker ps -aq --filter "volume=$volume_name" 2>/dev/null || true)"
  [[ -n "$attached" ]] || break
  attempt=$((attempt + 1))
  sleep 1
done
[[ -z "${attached:-}" ]] || { m0_fail 'A01.08 sandbox container still references the volume after deletion'; m0_finish; }

if ! create_volume_sandbox; then
  m0_fail "A01.08 second volume-backed sandbox failed (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
second_sandbox_id="$M0_LAST_SANDBOX_ID"
if [[ "$second_sandbox_id" == "$first_sandbox_id" ]]; then
  m0_fail 'A01.08 sandbox recreation reused the deleted sandbox ID'
  exit 1
fi
second_endpoint_url="$M0_LAST_ENDPOINT_URL"
if m0_aio_exec "$second_endpoint_url" "test \"\$(id -un)\" = gem && test -w /home/gem/workspace && test \"\$(cat $quoted_marker_file)\" = $quoted_marker && printf '%s\\n' volume-persisted" \
  && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.data.exit_code == 0 and ((.data.output // "") | contains("volume-persisted"))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.08 same named volume retained the marker and remains writable by gem'
else
  m0_fail "A01.08 volume marker did not survive sandbox recreation (HTTP $M0_HTTP_CODE)"
fi

m0_note 'A01.09 disk-full behavior was not exercised; filling a shared Docker data partition is unsafe'
m0_finish
