#!/bin/sh
set -eu

nc -z localhost "${SANDBOX_SRV_PORT:-8091}"
if [ "${DISABLE_BROWSER:-false}" != true ]; then
    nc -z localhost "${BROWSER_REMOTE_DEBUGGING_PORT:-9222}"
fi
# Keep the live AIO API probe aligned with Portal readiness without exposing its key in curl arguments.
aio_status="$(printf 'header = "X-AIO-API-Key: %s"\n' "${SANDBOX_API_KEY:-}" |
    curl --config - --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 3 \
        http://127.0.0.1:8080/v1/shell/sessions)" || exit 1
[ "$aio_status" = 200 ]
/usr/local/bin/morde-runtime healthcheck
