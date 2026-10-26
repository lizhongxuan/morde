#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-03-osb
m0_require_osb_and_linux
m0_require_pinned_aio_image
m0_generate_aio_key

# Request metadata is added by m0_create_aio_sandbox_body in the sourced library.
# shellcheck disable=SC2034
M0_RUN_ID="m0-03-$(date +%s)-$$"
docker image inspect "$CH_AIO_IMAGE" >/dev/null 2>&1 || docker pull "$CH_AIO_IMAGE" >/dev/null || m0_fatal 'could not pull configured AIO image'
m0_capture_aio_container_ids
started_at="$SECONDS"
sandbox_body="$(m0_create_aio_sandbox_body)"
if ! m0_osb_create "$sandbox_body"; then
  m0_fail "A01.07 OpenSandbox sandbox creation failed (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
sandbox_id="$M0_LAST_SANDBOX_ID"

if m0_osb_wait_state "$sandbox_id" Running; then
  m0_pass 'A01.07 OpenSandbox sandbox reached Running'
else
  m0_fail 'A01.07 OpenSandbox sandbox did not reach Running before timeout'
  m0_finish
fi

if ! m0_osb_endpoint "$sandbox_id" 8080; then
  m0_fail "A01.07 AIO endpoint lookup failed (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
endpoint_url="$M0_LAST_ENDPOINT_URL"
if m0_aio_wait_ready "$endpoint_url"; then
  elapsed="$(m0_record_elapsed "$started_at")"
  m0_pass "A01.07 AIO endpoint reached ready in $elapsed"
else
  m0_fail "A01.07 AIO endpoint readiness failed (HTTP $M0_HTTP_CODE)"
  m0_finish
fi

m0_aio_check_surface "$endpoint_url" A01.07
if m0_check_created_sandbox_container; then
  created_container="$M0_LAST_CREATED_CONTAINER"
else
  created_container=''
fi

if m0_osb_delete "$sandbox_id"; then
  m0_pass 'A01.07 sandbox delete completed'
else
  m0_fail 'A01.07 sandbox deletion did not complete'
fi
container_removed=0
if [[ -n "$created_container" ]]; then
  attempt=0
  while ((attempt < 60)); do
    if ! docker container inspect "$created_container" >/dev/null 2>&1; then
      container_removed=1
      break
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
fi
if [[ "$container_removed" == 1 ]]; then
  m0_pass 'A01.07 sandbox container was removed from Docker'
elif [[ -n "$created_container" ]]; then
  m0_fail 'A01.07 sandbox Docker container remains after deletion'
fi

m0_finish
