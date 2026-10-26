#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-05-egress
m0_require_osb_and_linux
m0_require_pinned_aio_image
m0_generate_aio_key
m0_require python3 ip

CH_EGRESS_PUBLIC_URL="${CH_EGRESS_PUBLIC_URL:-https://example.com/}"
CH_METADATA_URL="${CH_METADATA_URL:-http://169.254.169.254/}"
CH_PRIVATE_TEST_PORT="${CH_PRIVATE_TEST_PORT:-18761}"

is_rfc1918() {
  python3 - "$1" <<'PY'
import ipaddress
import sys

address = ipaddress.ip_address(sys.argv[1])
networks = tuple(ipaddress.ip_network(value) for value in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
raise SystemExit(0 if address.version == 4 and any(address in network for network in networks) else 1)
PY
}

if [[ -n "${CH_HOST_PRIVATE_IP:-}" ]]; then
  host_private_ip="$CH_HOST_PRIVATE_IP"
else
  route_ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") {print $(i + 1); exit}}' || true)"
  host_private_ip=''
  if [[ -n "$route_ip" ]] && is_rfc1918 "$route_ip"; then
    host_private_ip="$route_ip"
  else
    while IFS= read -r candidate; do
      if is_rfc1918 "$candidate"; then
        host_private_ip="$candidate"
        break
      fi
    done < <(ip -4 -o addr show scope global | awk '{sub(/\/.*/, "", $4); print $4}')
  fi
fi
[[ -n "$host_private_ip" ]] || m0_fatal 'could not determine a host RFC1918 address; set CH_HOST_PRIVATE_IP'
is_rfc1918 "$host_private_ip" || m0_fatal 'CH_HOST_PRIVATE_IP must be an RFC1918 IPv4 address'
if ! [[ "$CH_PRIVATE_TEST_PORT" =~ ^[0-9]{1,5}$ ]] || ((CH_PRIVATE_TEST_PORT < 1024 || CH_PRIVATE_TEST_PORT > 65535)); then
  m0_fatal 'CH_PRIVATE_TEST_PORT must be between 1024 and 65535'
fi

cat >"$M0_TMPDIR/private_http_server.py" <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import sys

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"cloudhost-private-host-probe\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        pass

HTTPServer((sys.argv[1], int(sys.argv[2])), Handler).serve_forever()
PY
python3 "$M0_TMPDIR/private_http_server.py" "$host_private_ip" "$CH_PRIVATE_TEST_PORT" >/dev/null 2>&1 &
private_server_pid=$!
m0_egress_on_exit() {
  local status=$?
  trap - EXIT INT TERM
  kill "$private_server_pid" 2>/dev/null || true
  wait "$private_server_pid" 2>/dev/null || true
  if ! m0_cleanup; then
    status=1
  fi
  exit "$status"
}
trap m0_egress_on_exit EXIT

host_probe_url="http://${host_private_ip}:${CH_PRIVATE_TEST_PORT}/"
attempt=0
while ((attempt < 10)); do
  if curl --noproxy '*' --silent --show-error --connect-timeout 2 --max-time 3 --output "$M0_TMPDIR/host-probe" "$host_probe_url"; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
grep -qx 'cloudhost-private-host-probe' "$M0_TMPDIR/host-probe" || m0_fatal 'could not start the temporary host-private-IP probe server'

# No networkPolicy is sent: the root-managed bridge and nftables policy is the sole isolation path.
# shellcheck disable=SC2034
M0_RUN_ID="m0-05-$(date +%s)-$$"
docker image inspect "$CH_AIO_IMAGE" >/dev/null 2>&1 || docker pull "$CH_AIO_IMAGE" >/dev/null || m0_fatal 'could not pull configured AIO image'
m0_capture_aio_container_ids
started_at="$SECONDS"
sandbox_body="$(m0_create_aio_sandbox_body)"
if ! m0_osb_create "$sandbox_body"; then
  m0_fail "A01.10 OpenSandbox could not create the isolated cloudhost sandbox (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
sandbox_id="$M0_LAST_SANDBOX_ID"
if ! m0_osb_wait_state "$sandbox_id" Running; then
  m0_fail 'A01.10 isolated sandbox did not reach Running'
  m0_finish
fi
if ! m0_osb_endpoint "$sandbox_id" 8080 || ! m0_aio_wait_ready "$M0_LAST_ENDPOINT_URL"; then
  m0_fail "A01.10 isolated AIO endpoint was not ready (HTTP $M0_HTTP_CODE)"
  m0_finish
fi
m0_check_created_sandbox_container || true
m0_note "A01.10 isolated sandbox creation-to-ready time: $(m0_record_elapsed "$started_at")"

printf -v quoted_public_url '%q' "$CH_EGRESS_PUBLIC_URL"
if m0_aio_exec "$M0_LAST_ENDPOINT_URL" "curl --noproxy '*' --fail --silent --show-error --connect-timeout 5 --max-time 15 $quoted_public_url -o /dev/null" \
  && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.data.exit_code == 0' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.10 sandbox can reach the configured public URL directly'
else
  m0_fail "A01.10 sandbox could not reach the configured public URL directly (AIO HTTP $M0_HTTP_CODE)"
fi

printf -v quoted_host_url '%q' "$host_probe_url"
private_command="status=\$(curl --noproxy '*' --silent --show-error --connect-timeout 3 --max-time 5 --output /dev/null --write-out '%{http_code}' $quoted_host_url 2>/dev/null || true); case \"\$status\" in 000|403) printf 'blocked:%s\\n' \"\$status\" ;; *) printf 'reachable:%s\\n' \"\$status\"; exit 1 ;; esac"
if m0_aio_exec "$M0_LAST_ENDPOINT_URL" "$private_command" && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.data.exit_code == 0 and ((.data.output // "") | contains("blocked:"))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.10 sandbox cannot reach the host RFC1918 address'
else
  m0_fail 'A01.10 host RFC1918 probe was reachable or could not be evaluated'
fi

printf -v quoted_metadata_url '%q' "$CH_METADATA_URL"
metadata_command="status=\$(curl --noproxy '*' --silent --show-error --connect-timeout 3 --max-time 5 --output /dev/null --write-out '%{http_code}' $quoted_metadata_url 2>/dev/null || true); case \"\$status\" in 000|403) printf 'blocked:%s\\n' \"\$status\" ;; *) printf 'reachable:%s\\n' \"\$status\"; exit 1 ;; esac"
if m0_aio_exec "$M0_LAST_ENDPOINT_URL" "$metadata_command" && [[ "$M0_HTTP_CODE" == 200 ]] && jq -e '.data.exit_code == 0 and ((.data.output // "") | contains("blocked:"))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.10 sandbox cannot reach the metadata address'
else
  m0_fail 'A01.10 metadata probe was reachable or could not be evaluated'
fi

raw_socket_python='import socket,sys; checks=[(socket.AF_INET,socket.SOCK_RAW,socket.IPPROTO_ICMP),(socket.AF_PACKET,socket.SOCK_RAW,socket.htons(3))]; opened=[]; exec("for family,kind,proto in checks:\n try:\n  handle=socket.socket(family,kind,proto); handle.close(); opened.append(True)\n except OSError:\n  opened.append(False)"); print("raw-sockets-denied" if not any(opened) else "raw-socket-open"); sys.exit(0 if not any(opened) else 1)'
printf -v quoted_raw_socket_python '%q' "$raw_socket_python"
raw_socket_command="python3 -c $quoted_raw_socket_python"
if m0_aio_exec "$M0_LAST_ENDPOINT_URL" "$raw_socket_command" && [[ "$M0_HTTP_CODE" == 200 ]] \
  && jq -e '.data.exit_code == 0 and ((.data.output // "") | contains("raw-sockets-denied"))' "$M0_TMPDIR/response" >/dev/null; then
  m0_pass 'A01.10 sandbox cannot create raw IP or packet sockets'
else
  m0_fail 'A01.10 raw socket creation was possible or could not be evaluated'
fi

m0_finish
