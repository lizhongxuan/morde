#!/bin/bash
set -Eeuo pipefail

. /usr/local/lib/morde-runtime-bootstrap.sh

apps_store=/opt/morde/apps
if [ -L "$apps_store" ] || [ ! -d "$apps_store" ] || ! mountpoint -q "$apps_store"; then
    echo 'Morde application store must be a dedicated persistent mount at /opt/morde/apps.' >&2
    exit 1
fi
if ! chown root:root "$apps_store" || ! chmod 0755 "$apps_store"; then
    echo 'Morde application store must be writable by root for secure initialization.' >&2
    exit 1
fi
if [ "$(stat -c '%u:%g:%a' "$apps_store")" != '0:0:755' ]; then
    echo 'Morde application store permissions are unsafe after initialization.' >&2
    exit 1
fi
/usr/local/bin/morde-runtime restore-apps

morde_bootstrap_runtime_credential
morde_bootstrap_host_replica_credential

if [ "${AIO_USER:-${USER:-gem}}" != "gem" ]; then
    echo 'Morde profile safeguard requires the fixed AIO user gem.' >&2
    exit 1
fi

# OpenSandbox creates fresh Docker volumes as root; the pinned AIO image runs gem as 1000:1000.
gem_gid=1000
install -d -o root -g "$gem_gid" -m 2770 /home/gem/workspace

# AIO v1.11.0 rewrites several files under /home/gem during gem.sh startup.
# Preserve the caller's hook privately, then restore profile files at AIO's
# pre-services hook, after its config copy and before supervisord starts.
install -d -o root -g root -m 0700 /run/morde-aio-profile
if [ -n "${RUN_HOOK_PRE_SERVICES:-}" ]; then
    original_hook_tmp="$(mktemp /run/morde-aio-profile/.pre-hook.XXXXXX)"
    printf '%s' "$RUN_HOOK_PRE_SERVICES" > "$original_hook_tmp"
    chown root:root "$original_hook_tmp"
    chmod 0600 "$original_hook_tmp"
    mv -f "$original_hook_tmp" /run/morde-aio-profile/pre-hook
fi
export MORDE_AIO_ORIGINAL_PRE_HOOK_FILE=/run/morde-aio-profile/pre-hook

/usr/local/bin/morde-aio-profile-config prepare
unset RUN_HOOK_PRE_SERVICES
export RUN_HOOK_PRE_SERVICES='exec /usr/local/bin/morde-aio-profile-hook'

aio_pid=""
runtime_pid=""
runtime_restart_wait_pid=""

terminate_children() {
    trap - EXIT INT TERM
    if [ -n "$runtime_restart_wait_pid" ]; then
        kill -TERM "$runtime_restart_wait_pid" 2>/dev/null || true
    fi
    if [ -n "$runtime_pid" ]; then
        kill -TERM "$runtime_pid" 2>/dev/null || true
    fi
    if [ -n "$aio_pid" ]; then
        kill -TERM "$aio_pid" 2>/dev/null || true
    fi
    if [ -n "$runtime_pid" ]; then
        wait "$runtime_pid" 2>/dev/null || true
    fi
    if [ -n "$aio_pid" ]; then
        wait "$aio_pid" 2>/dev/null || true
    fi
    if [ -n "$runtime_restart_wait_pid" ]; then
        wait "$runtime_restart_wait_pid" 2>/dev/null || true
    fi
}
trap terminate_children EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

/opt/gem/run.sh "$@" &
aio_pid=$!

if [ -n "${DISPLAY:-}" ] && [ -x /usr/bin/xdpyinfo ]; then
    for ((attempt = 0; attempt < 30; attempt++)); do
        if /usr/bin/xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
            break
        fi
        if ! kill -0 "$aio_pid" 2>/dev/null; then
            break
        fi
        sleep 1
    done
fi

# Keep live sync checkpoints on the Host runtime volume across container recreation.
XDG_CACHE_HOME=/var/lib/morde-runtime/cache /usr/local/bin/morde-runtime &
runtime_pid=$!
runtime_started=$SECONDS

status=0
restart_delay=1
while true; do
    wait_status=0
    wait -n "$runtime_pid" "$aio_pid" || wait_status=$?
    if ! kill -0 "$aio_pid" 2>/dev/null; then
        status=$wait_status
        break
    fi
    runtime_pid=""

    # Keep AIO and desktop applications alive while the Morde control plane restarts.
    if [ "$((SECONDS - runtime_started))" -ge 60 ]; then
        restart_delay=1
    fi
    echo "morde-runtime exited; restarting in ${restart_delay}s" >&2
    sleep "$restart_delay" &
    runtime_restart_wait_pid=$!
    wait_status=0
    wait -n "$runtime_restart_wait_pid" "$aio_pid" || wait_status=$?
    if ! kill -0 "$aio_pid" 2>/dev/null; then
        status=$wait_status
        break
    fi
    runtime_restart_wait_pid=""

    restart_delay=$((restart_delay * 2))
    if [ "$restart_delay" -gt 30 ]; then
        restart_delay=30
    fi
    XDG_CACHE_HOME=/var/lib/morde-runtime/cache /usr/local/bin/morde-runtime &
    runtime_pid=$!
    runtime_started=$SECONDS
done

terminate_children
trap - EXIT
exit "$status"
