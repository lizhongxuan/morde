#!/usr/bin/env bash
set -euo pipefail

image="${IMAGE_TAG:-registry.local/cloudhost/host:1.0.0}"
platform="${IMAGE_PLATFORM:-linux/amd64}"
credential=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
replica_credential=BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
codex_auth_fixture='{"morde_test_fixture":"SYNTHETIC_NOT_A_CREDENTIAL"}'
smoke_id="${$}-${RANDOM}-${RANDOM}"
runtime_volume="morde-runtime-smoke-${smoke_id}"
profile_volume="morde-profile-smoke-${smoke_id}"
workspace_volume="morde-workspace-smoke-${smoke_id}"
apps_volume="morde-apps-smoke-${smoke_id}"
container=""
entrypoint="$(dirname "${BASH_SOURCE[0]}")/morde-host-entrypoint.sh"
apps_mount_line="$(grep -nF 'mountpoint -q "$apps_store"' "$entrypoint" | cut -d: -f1)"
restore_apps_line="$(grep -nFx '/usr/local/bin/morde-runtime restore-apps' "$entrypoint" | cut -d: -f1)"
aio_start_line="$(grep -nFx '/opt/gem/run.sh "$@" &' "$entrypoint" | cut -d: -f1)"
if [[ -z "$apps_mount_line" || -z "$restore_apps_line" || -z "$aio_start_line" ||
      "$apps_mount_line" -ge "$restore_apps_line" || "$restore_apps_line" -ge "$aio_start_line" ]]; then
  echo "application store recovery must run after mount validation and before AIO" >&2
  exit 1
fi
cleanup() {
  if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null 2>&1 || true; fi
  docker volume rm "$runtime_volume" >/dev/null 2>&1 || true
  docker volume rm "$profile_volume" >/dev/null 2>&1 || true
  docker volume rm "$workspace_volume" >/dev/null 2>&1 || true
  docker volume rm "$apps_volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT
for volume in "$runtime_volume" "$profile_volume" "$workspace_volume" "$apps_volume"; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    echo "refuse to reuse existing smoke volume $volume" >&2
    exit 1
  fi
done
docker volume create "$runtime_volume" >/dev/null
docker volume create "$profile_volume" >/dev/null
docker volume create "$workspace_volume" >/dev/null
docker volume create "$apps_volume" >/dev/null

if missing_apps_output="$(docker run --rm --platform "$platform" --network none \
  --entrypoint /usr/local/bin/morde-host-entrypoint "$image" 2>&1)"; then
  echo "Host unexpectedly started without the persistent application store mount" >&2
  exit 1
fi
if ! grep -Fq 'dedicated persistent mount at /opt/morde/apps' <<<"$missing_apps_output"; then
  echo "Host failed for an unexpected reason without the application store mount" >&2
  printf '%s\n' "$missing_apps_output" >&2
  exit 1
fi
for apps_setup in 'mkdir -p /opt/morde/apps' 'mkdir -p /opt/morde && ln -s /tmp /opt/morde/apps'; do
  if invalid_apps_output="$(docker run --rm --platform "$platform" --network none \
    --entrypoint /bin/sh "$image" -c "set -e; $apps_setup; exec /usr/local/bin/morde-host-entrypoint" 2>&1)"; then
    echo "Host unexpectedly started with an invalid application store path: $apps_setup" >&2
    exit 1
  fi
  if ! grep -Fq 'dedicated persistent mount at /opt/morde/apps' <<<"$invalid_apps_output"; then
    echo "Host failed for an unexpected reason with an invalid application store path: $apps_setup" >&2
    printf '%s\n' "$invalid_apps_output" >&2
    exit 1
  fi
done

docker run --rm --platform "$platform" --network none \
  --mount "type=volume,src=$apps_volume,dst=/opt/morde/apps" \
  --entrypoint /bin/sh "$image" -c 'printf installed-app > /opt/morde/apps/smoke-marker && chmod 0777 /opt/morde/apps'
docker run --rm --platform "$platform" --network none \
  --mount "type=volume,src=$profile_volume,dst=/home/gem" \
  --entrypoint /bin/sh "$image" -c '
  mkdir -p /home/gem/.config/code-server/vscode/User /home/gem/.config/matplotlib /home/gem/.config/browser/Default
  printf "user-profile-bashrc\n" > /home/gem/.bashrc
  printf "{\"morde_profile\":\"user-code-settings\"}\n" > /home/gem/.config/code-server/vscode/User/settings.json
  printf "# user-profile-matplotlib\n" > /home/gem/.config/matplotlib/matplotlibrc
  printf "{\"morde_profile\":\"user-browser-preferences\"}\n" > /home/gem/.config/browser/Default/Preferences
  chown -R 1000:1000 /home/gem/.bashrc /home/gem/.config
'
# GUI readiness and Chromium sandboxing are verified separately on isolated Linux.
container="$(docker run -d --platform "$platform" --network none \
  --mount "type=volume,src=$profile_volume,dst=/home/gem" \
  --mount "type=volume,src=$workspace_volume,dst=/home/gem/workspace" \
  --mount "type=volume,src=$runtime_volume,dst=/var/lib/morde-runtime" \
  --mount "type=volume,src=$apps_volume,dst=/opt/morde/apps" \
  --env "MORDE_RUNTIME_BOOTSTRAP_KEY=$credential" \
  --env "MORDE_HOST_REPLICA_KEY=$replica_credential" \
  --env MORDE_HOST_ID=h1234567 \
  --env MORDE_RUNTIME_GENERATION=1 \
  --env SANDBOX_API_KEY=synthetic-aio-key \
  --env DISABLE_BROWSER=true "$image")"

ready=false
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  state="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)"
  if [[ "$state" != running ]]; then
    echo "host container stopped before AIO became ready" >&2
    exit 1
  fi
  if docker exec "$container" /usr/local/bin/cloudhost-healthcheck >/dev/null 2>&1 && \
     docker exec "$container" sh -c 'test -n "$DISPLAY" && /usr/bin/xdpyinfo -display "$DISPLAY" >/dev/null 2>&1'; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "AIO did not become ready within 90 seconds" >&2
  exit 1
fi
if docker exec --env SANDBOX_API_KEY=wrong-key "$container" /usr/local/bin/cloudhost-healthcheck >/dev/null 2>&1; then
  echo "Host healthcheck accepted an invalid AIO API key" >&2
  exit 1
fi
docker exec "$container" sh -c 'test "$(stat -c "%u:%g:%a" /opt/morde/apps)" = 0:0:755 && test "$(cat /opt/morde/apps/smoke-marker)" = installed-app'
if docker logs "$container" 2>&1 | grep -Fq 'morde-runtime: desktop capabilities unavailable'; then
  echo "desktop capability was unavailable during AIO startup" >&2
  exit 1
fi

runtime_pid="$(docker exec "$container" python3 -c 'import os
for value in os.listdir("/proc"):
    if not value.isdigit():
        continue
    try:
        args = open(f"/proc/{value}/cmdline", "rb").read().split(b"\0")
    except OSError:
        continue
    if args == [b"/usr/local/bin/morde-runtime", b""]:
        print(value)
        break')"
if [[ -z "$runtime_pid" ]]; then
  echo "could not locate the running Morde Runtime process" >&2
  exit 1
fi
docker exec "$container" kill -TERM "$runtime_pid"
recovered=false
deadline=$((SECONDS + 45))
while (( SECONDS < deadline )); do
  state="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)"
  if [[ "$state" != running ]]; then
    echo "Host container stopped when Morde Runtime exited" >&2
    exit 1
  fi
  if docker exec "$container" /usr/local/bin/cloudhost-healthcheck >/dev/null 2>&1; then
    recovered=true
    break
  fi
  sleep 1
done
if [[ "$recovered" != true ]]; then
  echo "Morde Runtime did not recover while AIO remained available" >&2
  exit 1
fi

docker exec --env MORDE_RUNTIME_BOOTSTRAP_KEY= --env MORDE_HOST_REPLICA_KEY= "$container" sh -c '
  key_file=/run/morde-runtime/credential
  test "$(stat -c %u:%g:%a "$key_file")" = 0:0:400
  test "$(cat "$key_file")" = "$1"
  replica_file=/run/morde-runtime/replica-credential
  test "$(stat -c %u:%g:%a "$replica_file")" = 0:0:400
  test "$(cat "$replica_file")" = "$2"
  test "$(stat -c %u:%g:%a /var/lib/morde-runtime)" = 0:0:700
  printf persistent-runtime-data > /var/lib/morde-runtime/smoke-marker
  for process_env in /proc/[0-9]*/environ; do
    process_uid="$(stat -c %u "${process_env%/environ}" 2>/dev/null || true)"
    # The trusted root entrypoint must read the bootstrap value to persist the
    # credential. Verify that no unprivileged application process inherits it.
    if [ "$process_uid" = 0 ] || [ -z "$process_uid" ]; then
      continue
    fi
    if tr "\000" "\n" 2>/dev/null <"$process_env" | grep -Eq "^MORDE_(RUNTIME_BOOTSTRAP_KEY|HOST_REPLICA_KEY)=." 2>/dev/null; then
      exit 1
    fi
  done
' sh "$credential" "$replica_credential"
docker exec --env MORDE_RUNTIME_BOOTSTRAP_KEY= --env MORDE_HOST_REPLICA_KEY= --user 1000:1000 "$container" sh -c 'test ! -r /run/morde-runtime/credential && test ! -r /run/morde-runtime/replica-credential'
docker exec --user 0:0 "$container" sh -c '
  test "$(stat -c %u:%g:%a /home/gem/workspace)" = 0:1000:2770
  printf runtime-created > /home/gem/workspace/runtime-created-by-root
  chmod 0660 /home/gem/workspace/runtime-created-by-root
'
docker exec --user 1000:1000 "$container" sh -c '
  printf "%s" "-gem-edit" >> /home/gem/workspace/runtime-created-by-root
  test "$(cat /home/gem/workspace/runtime-created-by-root)" = runtime-created-gem-edit
'
docker exec --user 1000:1000 "$container" sh -c '
  test "$(cat /home/gem/.bashrc)" = user-profile-bashrc
  grep -q user-code-settings /home/gem/.config/code-server/vscode/User/settings.json
  grep -q user-profile-matplotlib /home/gem/.config/matplotlib/matplotlibrc
  grep -q user-browser-preferences /home/gem/.config/browser/Default/Preferences
'
docker exec --user 1000:1000 "$container" sh -c '
  codex --version | grep -F 0.164.0
  if codex login status >/dev/null 2>&1; then
    echo "fresh profile unexpectedly has Codex credentials" >&2
    exit 1
  fi
'
docker exec --user 1000:1000 "$container" sh -c '
  mkdir -p /home/gem/.local/share /home/gem/workspace
  printf persistent-profile > /home/gem/.local/share/morde-smoke-marker
  printf persistent-workspace > /home/gem/workspace/morde-smoke-marker
  printf user-edited-profile-bashrc > /home/gem/.bashrc
  printf "{\"morde_profile\":\"user-edited-code-settings\"}" > /home/gem/.config/code-server/vscode/User/settings.json
  mkdir -p /home/gem/.codex
  printf "%s\n" "$1" > /home/gem/.codex/auth.json
  chmod 0600 /home/gem/.codex/auth.json
' sh "$codex_auth_fixture"
docker rm -f "$container" >/dev/null
container=""
container="$(docker run -d --platform "$platform" --network none \
  --mount "type=volume,src=$profile_volume,dst=/home/gem" \
  --mount "type=volume,src=$workspace_volume,dst=/home/gem/workspace" \
  --mount "type=volume,src=$runtime_volume,dst=/var/lib/morde-runtime" \
  --mount "type=volume,src=$apps_volume,dst=/opt/morde/apps" \
  --env "MORDE_RUNTIME_BOOTSTRAP_KEY=$credential" \
  --env "MORDE_HOST_REPLICA_KEY=$replica_credential" \
  --env MORDE_HOST_ID=h1234567 \
  --env MORDE_RUNTIME_GENERATION=1 \
  --env SANDBOX_API_KEY=synthetic-aio-key \
  --env DISABLE_BROWSER=true "$image")"
ready=false
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  state="$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)"
  if [[ "$state" != running ]]; then
    echo "host container stopped before second AIO startup completed" >&2
    exit 1
  fi
  if docker exec "$container" /usr/local/bin/cloudhost-healthcheck >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "AIO did not become ready after container recreation" >&2
  exit 1
fi
docker exec "$container" sh -c 'test "$(stat -c "%u:%g:%a" /opt/morde/apps)" = 0:0:755 && test "$(cat /opt/morde/apps/smoke-marker)" = installed-app'
docker exec --user 1000:1000 "$container" sh -c '
  test "$(cat /home/gem/.local/share/morde-smoke-marker)" = persistent-profile
  test "$(cat /home/gem/workspace/morde-smoke-marker)" = persistent-workspace
  test "$(cat /home/gem/.bashrc)" = user-edited-profile-bashrc
  grep -q user-edited-code-settings /home/gem/.config/code-server/vscode/User/settings.json
  test "$(cat /home/gem/.codex/auth.json)" = "$1"
  test "$(stat -c %u:%g:%a /home/gem/.codex/auth.json)" = 1000:1000:600
' sh "$codex_auth_fixture"
docker exec --env MORDE_RUNTIME_BOOTSTRAP_KEY= --env MORDE_HOST_REPLICA_KEY= "$container" sh -c '
  test "$(stat -c %u:%g:%a /var/lib/morde-runtime)" = 0:0:700
  test "$(cat /var/lib/morde-runtime/smoke-marker)" = persistent-runtime-data
'
docker exec --env MORDE_RUNTIME_BOOTSTRAP_KEY= --env MORDE_HOST_REPLICA_KEY= "$container" sh -c '
  key_file=/run/morde-runtime/credential
  test "$(cat "$key_file")" = "$1"
  test "$(stat -c %u:%g:%a "$key_file")" = 0:0:400
' sh "$credential"
docker rm -f "$container" >/dev/null
container=""
printf 'PASS: Host refused to start without its dedicated application store, repaired the mounted store permissions, and kept the store, profile, Workspace, and runtime volumes separate across container recreation\n'
