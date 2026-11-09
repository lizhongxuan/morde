#!/usr/bin/env bash

set -Eeuo pipefail

readonly NETWORK_NAME="morde-task-sandbox"
readonly BRIDGE_NAME="mtbr0"
readonly SUBNET="172.31.0.0/24"
readonly GATEWAY="172.31.0.1"
readonly CONFIG_PATH="/etc/opensandbox/opensandbox-task.toml"
readonly DOCKER_CONFIG_PATH="/etc/docker/daemon.json"
readonly RESOLVER_PATH="/etc/cloudhost/task-sandbox-resolv.conf"
readonly STATE_DIR="/etc/cloudhost/task-sandbox-network-policy"
readonly NFT_BIN="/usr/sbin/nft"
readonly DOCKER_HOST_URI="unix:///var/run/docker.sock"
readonly OSB_SERVICE="opensandbox-task-server"
readonly OSB_CONFIG_DEST="/etc/opensandbox/opensandbox-task.toml"
readonly EXEC_PORT_RANGE_MIN=46000
readonly EXEC_PORT_RANGE_MAX=46999
readonly EXECD_PORT=44772

TMP_DIR=""

fail() {
  printf 'FAIL task-sandbox-network-policy %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS task-sandbox-network-policy %s\n' "$*"
}

cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
}

docker_cli() {
  DOCKER_HOST="$DOCKER_HOST_URI" docker "$@"
}

require_linux() {
  [[ "$(uname -s)" == Linux ]] || fail 'Linux host required'
  [[ -x "$NFT_BIN" ]] || fail 'nftables executable missing at /usr/sbin/nft'
  command -v python3 >/dev/null 2>&1 || fail 'Python 3 missing'
  python3 -c 'import tomllib' >/dev/null 2>&1 || fail 'Python 3.11 or later with tomllib is required'
  command -v docker >/dev/null 2>&1 || fail 'Docker CLI missing'
  command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum missing'
}

require_root() {
  (( EUID == 0 )) || fail 'task sandbox policy actions must run as root'
}

protect_file() {
  python3 - "$1" <<'PY'
import os
import stat
import sys

path = os.path.abspath(sys.argv[1])
parent = os.path.dirname(path)
while True:
    try:
        info = os.lstat(parent)
    except OSError as exc:
        raise SystemExit("FAIL task-sandbox-network-policy protected directory is unavailable") from exc
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise SystemExit("FAIL task-sandbox-network-policy protected directories must be root-owned and not group/world-writable")
    if parent == "/":
        break
    parent = os.path.dirname(parent)
try:
    info = os.lstat(path)
except OSError as exc:
    raise SystemExit("FAIL task-sandbox-network-policy protected file is unavailable") from exc
if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
    raise SystemExit("FAIL task-sandbox-network-policy protected file must be root-owned and not group/world-writable")
if not os.access(path, os.R_OK):
    raise SystemExit("FAIL task-sandbox-network-policy protected file is not readable")
PY
}

config_preflight() {
  protect_file "$CONFIG_PATH"
  protect_file "$DOCKER_CONFIG_PATH"
  protect_file "$RESOLVER_PATH"
  python3 - "$CONFIG_PATH" "$DOCKER_CONFIG_PATH" "$RESOLVER_PATH" <<'PY'
import json
import re
import sys
import tomllib

config_path, daemon_path, resolver_path = sys.argv[1:]
try:
    with open(config_path, "rb") as source:
        config = tomllib.load(source)
    with open(daemon_path, encoding="utf-8") as source:
        daemon = json.load(source)
    with open(resolver_path, encoding="ascii") as source:
        resolver = source.read().splitlines()
except (OSError, UnicodeError, json.JSONDecodeError, tomllib.TOMLDecodeError) as exc:
    raise SystemExit("FAIL task-sandbox-network-policy protected config could not be parsed") from exc

server = config.get("server", {})
proxy = config.get("proxy", {})
runtime = config.get("runtime", {})
docker = config.get("docker", {})
secure = config.get("secure_runtime", {})
image = runtime.get("execd_image", "")
if server != {"host": "127.0.0.1", "port": 18081, "eip": "127.0.0.1"}:
    raise SystemExit("FAIL task-sandbox-network-policy task OpenSandbox API must be loopback-only on port 18081")
if proxy.get("resolve_internal") is not False:
    raise SystemExit("FAIL task-sandbox-network-policy task OpenSandbox internal proxy resolution must be disabled")
if runtime.get("type") != "docker" or not re.fullmatch(
    r"docker\.io/opensandbox/execd:release-1\.1\.1@sha256:[0-9a-f]{64}", image
):
    raise SystemExit("FAIL task-sandbox-network-policy task Execd image must be pinned to release 1.1.1 by digest")
if secure != {"type": "gvisor", "docker_runtime": "runsc"}:
    raise SystemExit("FAIL task-sandbox-network-policy task sandbox must use Docker runsc")
if (
    docker.get("network_mode") != "morde-task-sandbox"
    or docker.get("publish_host") != "127.0.0.1"
    or docker.get("port_range_min") != 46000
    or docker.get("port_range_max") != 46999
    or docker.get("pids_limit") != 64
    or docker.get("no_new_privileges") is not True
    or docker.get("drop_capabilities") != ["NET_RAW"]
    or docker.get("sandbox_binds") != [f"{resolver_path}:/etc/resolv.conf:ro"]
    or docker.get("sandbox_env") != {}
    or docker.get("seccomp_profile") is not None
    or docker.get("apparmor_profile") is not None
):
    raise SystemExit("FAIL task-sandbox-network-policy task Docker sandbox configuration is not the reviewed profile")
if "egress" in config or "networkPolicy" in config:
    raise SystemExit("FAIL task-sandbox-network-policy task config must not enable an egress sidecar or networkPolicy")
if resolver != ["nameserver 127.0.0.1", "options attempts:1 timeout:1"]:
    raise SystemExit("FAIL task-sandbox-network-policy task resolver must have no upstream DNS server")

if daemon.get("userland-proxy") is not False:
    raise SystemExit("FAIL task-sandbox-network-policy Docker userland-proxy must be disabled for the loopback DNAT contract")
runsc = (daemon.get("runtimes") or {}).get("runsc")
if not isinstance(runsc, dict) or runsc.get("runtimeArgs") != ["--net-raw=false", "--allow-packet-socket-write=false"]:
    raise SystemExit("FAIL task-sandbox-network-policy Docker runsc packet-socket restrictions do not match")
PY
}

live_docker_runtime_preflight() {
  local runtimes
  runtimes="$(docker_cli info --format '{{json .Runtimes}}' 2>/dev/null)" || fail 'Docker daemon is unavailable'
  python3 - "$runtimes" <<'PY'
import json
import sys

try:
    runtimes = json.loads(sys.argv[1])
except (IndexError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL task-sandbox-network-policy live Docker runtime report is invalid") from exc
runsc = runtimes.get("runsc") if isinstance(runtimes, dict) else None
if not isinstance(runsc, dict) or runsc.get("runtimeArgs") != ["--net-raw=false", "--allow-packet-socket-write=false"]:
    raise SystemExit("FAIL task-sandbox-network-policy live Docker runsc runtime does not match protected configuration")
PY
}

live_network_preflight() {
  local network_json
  network_json="$(docker_cli network inspect --format '{{json .}}' "$NETWORK_NAME" 2>/dev/null)" || fail 'dedicated task sandbox Docker network is missing'
  python3 - "$network_json" <<'PY'
import ipaddress
import json
import sys

try:
    network = json.loads(sys.argv[1])
except (IndexError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL task-sandbox-network-policy live Docker network report is invalid") from exc
expected_options = {
    "com.docker.network.bridge.name": "mtbr0",
    "com.docker.network.bridge.enable_icc": "false",
    "com.docker.network.bridge.enable_ip_masquerade": "false",
    "com.docker.network.bridge.host_binding_ipv4": "127.0.0.1",
}
ipam = network.get("IPAM") or {}
subnets = [entry.get("Subnet") for entry in ipam.get("Config", [])]
gateways = [entry.get("Gateway") for entry in ipam.get("Config", [])]
try:
    subnet = ipaddress.ip_network(subnets[0], strict=True) if len(subnets) == 1 else None
except ValueError:
    subnet = None
if (
    network.get("Name") != "morde-task-sandbox"
    or network.get("Driver") != "bridge"
    or network.get("Scope") != "local"
    or network.get("EnableIPv6") is not False
    or network.get("Internal") is True
    or any((network.get("Options") or {}).get(key) != value for key, value in expected_options.items())
    or subnet is None
    or str(subnet) != "172.31.0.0/24"
    or subnet.version != 4
    or gateways != ["172.31.0.1"]
):
    raise SystemExit("FAIL task-sandbox-network-policy live task bridge settings do not match the protected profile")
PY
}

ensure_network() {
  local network_json
  if network_json="$(docker_cli network inspect --format '{{json .}}' "$NETWORK_NAME" 2>/dev/null)"; then
    live_network_preflight
    pass 'existing dedicated task bridge matches the protected profile'
    return
  fi

  # A name collision must fail closed. Never replace a network that Docker
  # already knows about or that might have been created with weaker settings.
  docker_cli network create \
    --driver bridge \
    --subnet "$SUBNET" \
    --gateway "$GATEWAY" \
    --opt "com.docker.network.bridge.name=$BRIDGE_NAME" \
    --opt com.docker.network.bridge.enable_icc=false \
    --opt com.docker.network.bridge.enable_ip_masquerade=false \
    --opt com.docker.network.bridge.host_binding_ipv4=127.0.0.1 \
    "$NETWORK_NAME" >/dev/null || fail 'could not create the dedicated task sandbox Docker bridge'
  live_network_preflight
  pass 'created and verified the dedicated task sandbox bridge'
}

live_task_server_preflight() {
  local ids container_id inspect_json
  ids="$(docker_cli ps --quiet --filter "label=com.docker.compose.project=cloudhost" --filter "label=com.docker.compose.service=$OSB_SERVICE" 2>/dev/null)" || fail 'could not inspect the task OpenSandbox service'
  [[ "$(printf '%s\n' "$ids" | sed '/^$/d' | wc -l | tr -d ' ')" == 1 ]] || fail 'exactly one running task OpenSandbox service is required'
  container_id="$(printf '%s\n' "$ids" | sed '/^$/d')"
  [[ "$container_id" =~ ^[[:xdigit:]]{12,64}$ ]] || fail 'task OpenSandbox container identifier is invalid'
  inspect_json="$(docker_cli inspect "$container_id" 2>/dev/null)" || fail 'could not inspect the task OpenSandbox container'
  python3 - "$CONFIG_PATH" "$OSB_CONFIG_DEST" "$inspect_json" <<'PY'
import datetime
import json
import os
import sys

config_path, config_dest, payload = sys.argv[1:]
try:
    values = json.loads(payload)
except json.JSONDecodeError as exc:
    raise SystemExit("FAIL task-sandbox-network-policy live task OpenSandbox inspection is invalid") from exc
if not isinstance(values, list) or len(values) != 1:
    raise SystemExit("FAIL task-sandbox-network-policy live task OpenSandbox inspection is ambiguous")
container = values[0]
mounts = [item for item in container.get("Mounts", []) if item.get("Destination") == config_dest]
state = container.get("State") or {}
if (
    container.get("Path", "").rsplit("/", 1)[-1] != "opensandbox-server"
    or container.get("Args") != ["--config", config_dest]
    or (container.get("HostConfig") or {}).get("NetworkMode") != "host"
    or len(mounts) != 1
    or os.path.realpath(mounts[0].get("Source", "")) != os.path.realpath(config_path)
    or mounts[0].get("RW") is not False
):
    raise SystemExit("FAIL task OpenSandbox must use its protected read-only config with host control-plane networking")
try:
    started = datetime.datetime.fromisoformat(state["StartedAt"].replace("Z", "+00:00"))
    modified = os.stat(config_path, follow_symlinks=False).st_mtime_ns
except (KeyError, OSError, ValueError) as exc:
    raise SystemExit("FAIL task OpenSandbox start time or protected config timestamp is unavailable") from exc
if started.timestamp() * 1_000_000_000 < modified:
    raise SystemExit("FAIL task OpenSandbox started before its current protected config")
PY
}

build_policy_rules() {
  cat >"$TMP_DIR/rules.nft" <<NFT
destroy table inet morde_task_sandbox
destroy table bridge morde_task_sandbox
add table inet morde_task_sandbox
add chain inet morde_task_sandbox input { type filter hook input priority filter - 1; policy accept; }
add chain inet morde_task_sandbox output { type filter hook output priority filter - 1; policy accept; }
add chain inet morde_task_sandbox forward { type filter hook forward priority filter - 1; policy accept; }
# Only an established reply to a host-created loopback DNAT Execd request may reach host input.
add rule inet morde_task_sandbox input iifname "$BRIDGE_NAME" ct state established ct direction reply ct original ip saddr 127.0.0.1 ct original ip daddr 127.0.0.1 ct original proto-dst $EXEC_PORT_RANGE_MIN-$EXEC_PORT_RANGE_MAX tcp sport $EXECD_PORT counter accept
add rule inet morde_task_sandbox input iifname "$BRIDGE_NAME" counter drop
# With Docker userland-proxy disabled, host loopback port publication is a DNATed OUTPUT flow.
# Bind address, original tuple, allocated host-port range and final Execd port must all match.
add rule inet morde_task_sandbox output oifname "$BRIDGE_NAME" ct status dnat ct state new ct direction original ct original ip saddr 127.0.0.1 ct original ip daddr 127.0.0.1 ct original proto-dst $EXEC_PORT_RANGE_MIN-$EXEC_PORT_RANGE_MAX ip daddr $SUBNET tcp dport $EXECD_PORT counter accept
add rule inet morde_task_sandbox output oifname "$BRIDGE_NAME" counter drop
# No task-originated forwarding, including public IP, DNS, metadata, private IP or IPv6.
add rule inet morde_task_sandbox forward iifname "$BRIDGE_NAME" counter drop
# No externally initiated forwarding to a task. Loopback DNAT is handled in OUTPUT above.
add rule inet morde_task_sandbox forward oifname "$BRIDGE_NAME" counter drop
add table bridge morde_task_sandbox
add chain bridge morde_task_sandbox forward { type filter hook forward priority filter - 1; policy accept; }
add rule bridge morde_task_sandbox forward ibrname "$BRIDGE_NAME" obrname "$BRIDGE_NAME" counter drop
NFT
}

check_state_directory() {
  local path="/etc" component
  [[ -d /etc && ! -L /etc && "$(stat -c '%u' /etc)" == 0 ]] || fail '/etc must be a root-owned real directory'
  [[ -d /etc/cloudhost && ! -L /etc/cloudhost && "$(stat -c '%u' /etc/cloudhost)" == 0 ]] || fail '/etc/cloudhost must be a root-owned real directory'
  [[ ! -L "$STATE_DIR" ]] || fail 'task sandbox state directory must not be a symlink'
  if [[ -e "$STATE_DIR" ]]; then
    [[ -d "$STATE_DIR" && "$(stat -c '%u' "$STATE_DIR")" == 0 ]] || fail 'task sandbox state path must be a root-owned directory'
    local mode
    mode="$(stat -c '%a' "$STATE_DIR")" || fail 'cannot inspect task sandbox state directory permissions'
    (( (8#$mode & 8#022) == 0 )) || fail 'task sandbox state directory is writable by group or others'
  fi
}

prepare_state_directory() {
  check_state_directory
  if [[ ! -d "$STATE_DIR" ]]; then
    install -d -o root -g root -m 0755 "$STATE_DIR"
  fi
  chown root:root "$STATE_DIR"
  chmod 0755 "$STATE_DIR"
}

lock_policy() {
  local lock_path="/run/lock/morde-task-sandbox-network-policy.lock"
  command -v flock >/dev/null 2>&1 || fail 'flock is required for policy changes'
  [[ -d /run/lock && ! -L "$lock_path" ]] || fail 'task sandbox policy lock path is invalid'
  if [[ ! -e "$lock_path" ]]; then
    install -o root -g root -m 0600 /dev/null "$lock_path"
  fi
  [[ "$(stat -c '%u' "$lock_path")" == 0 ]] || fail 'task sandbox policy lock must be root-owned'
  local mode
  mode="$(stat -c '%a' "$lock_path")" || fail 'cannot inspect task sandbox lock permissions'
  (( (8#$mode & 8#022) == 0 )) || fail 'task sandbox policy lock is writable by group or others'
  exec 9>"$lock_path"
  flock -x 9
}

make_temp_directory() {
  TMP_DIR="$(mktemp -d /tmp/task-sandbox-network-policy.XXXXXX)" || fail 'cannot create temporary policy directory'
  chmod 0700 "$TMP_DIR"
}

table_fingerprint() {
  "$NFT_BIN" -j list table inet morde_task_sandbox >"$TMP_DIR/inet.json" 2>/dev/null || fail 'task sandbox inet nftables table is missing'
  "$NFT_BIN" -j list table bridge morde_task_sandbox >"$TMP_DIR/bridge.json" 2>/dev/null || fail 'task sandbox bridge nftables table is missing'
  python3 - "$TMP_DIR/inet.json" "$TMP_DIR/bridge.json" <<'PY'
import hashlib
import json
import sys

def canonical(path):
    with open(path, encoding="utf-8") as source:
        value = json.load(source)
    entries = value.get("nftables")
    if not isinstance(entries, list):
        raise ValueError("nftables JSON is invalid")
    def strip_handles(item):
        if isinstance(item, list):
            return [strip_handles(entry) for entry in item]
        if isinstance(item, dict):
            normalized = {}
            for key, val in item.items():
                if key in {"handle", "metainfo"}:
                    continue
                if key == "counter" and isinstance(val, dict):
                    # Packet and byte totals change during normal traffic and
                    # must not look like policy drift.
                    normalized[key] = {}
                    continue
                normalized[key] = strip_handles(val)
            return normalized
        return item
    return json.dumps(strip_handles(entries), sort_keys=True, separators=(",", ":"))

try:
    digest = hashlib.sha256((canonical(sys.argv[1]) + canonical(sys.argv[2])).encode()).hexdigest()
except (OSError, json.JSONDecodeError, ValueError, IndexError):
    raise SystemExit("FAIL task-sandbox-network-policy cannot fingerprint live policy")
print(digest)
PY
}

config_fingerprint() {
  local network_json="${1:-}" network_digest=""
  if [[ -n "$network_json" ]]; then
    network_digest="$(python3 - "$network_json" <<'PY'
import hashlib
import json
import sys

try:
    network = json.loads(sys.argv[1])
except (IndexError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL task-sandbox-network-policy cannot fingerprint Docker network") from exc
selected = {
    "Name": network.get("Name"),
    "Driver": network.get("Driver"),
    "Scope": network.get("Scope"),
    "EnableIPv6": network.get("EnableIPv6"),
    "Internal": network.get("Internal"),
    "Options": network.get("Options"),
    "IPAM": network.get("IPAM"),
}
print(hashlib.sha256(json.dumps(selected, sort_keys=True, separators=(",", ":")).encode()).hexdigest())
PY
)" || fail 'cannot fingerprint task sandbox Docker network'
  fi
  {
    sha256sum "$CONFIG_PATH" "$DOCKER_CONFIG_PATH" || fail 'cannot fingerprint protected task sandbox configuration'
    printf '%s\n' "$network_digest"
  } | sha256sum | awk '{print $1}'
}

write_state_file() {
  local name="$1" value="$2" tmp
  prepare_state_directory
  tmp="$(mktemp "$STATE_DIR/.${name}.XXXXXX")" || fail 'cannot stage task sandbox policy state'
  printf '%s\n' "$value" >"$tmp" || fail 'cannot write task sandbox policy state'
  chown root:root "$tmp"
  chmod 0644 "$tmp"
  mv -f -- "$tmp" "$STATE_DIR/$name"
}

read_state_file() {
  local path="$STATE_DIR/$1" mode
  [[ -f "$path" && ! -L "$path" && "$(stat -c '%u' "$path" 2>/dev/null)" == 0 ]] || fail "root-protected task sandbox state $1 is missing"
  mode="$(stat -c '%a' "$path" 2>/dev/null)" || fail "cannot inspect task sandbox state $1"
  (( (8#$mode & 8#022) == 0 )) || fail "task sandbox state $1 is writable by group or others"
  cat -- "$path"
}

assert_preloaded_policy() {
  check_state_directory
  local expected_config expected_rules actual_config actual_rules
  expected_config="$(read_state_file config-fingerprint)"
  expected_rules="$(read_state_file nft-fingerprint)"
  [[ "$expected_config" =~ ^[[:xdigit:]]{64}$ && "$expected_rules" =~ ^[[:xdigit:]]{64}$ ]] || fail 'preloaded task sandbox policy state is malformed'
  actual_config="$(config_fingerprint)"
  [[ "$actual_config" == "$expected_config" ]] || fail 'task sandbox protected configuration changed after preload'
  actual_rules="$(table_fingerprint)"
  [[ "$actual_rules" == "$expected_rules" ]] || fail 'task sandbox nftables policy drifted after preload; stop task sandboxes and run preload'
}

ensure_task_sandbox_idle() {
  [[ -S /var/run/docker.sock ]] || return 0
  docker_cli info >/dev/null 2>&1 || fail 'Docker socket exists but the daemon could not be inspected'
  local task_service_ids task_container_ids
  task_service_ids="$(docker_cli ps -aq --filter "label=com.docker.compose.project=cloudhost" --filter "label=com.docker.compose.service=$OSB_SERVICE" 2>/dev/null)" || fail 'could not inspect task OpenSandbox containers'
  task_container_ids="$(docker_cli ps -aq --filter "network=$NETWORK_NAME" 2>/dev/null)" || fail 'could not inspect task sandbox network attachments'
  [[ -z "$task_service_ids" && -z "$task_container_ids" ]] || fail 'preload requires the task OpenSandbox service stopped and no containers attached to the task network'
}

apply_policy() {
  build_policy_rules
  "$NFT_BIN" --check --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables rejected the isolated Task Sandbox policy; no rules were changed'
  "$NFT_BIN" --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables could not atomically replace only the Task Sandbox policy tables'
  local rules_fingerprint
  rules_fingerprint="$(table_fingerprint)"
  write_state_file nft-fingerprint "$rules_fingerprint"
}

preload_policy() {
  config_preflight
  ensure_task_sandbox_idle
  check_state_directory
  apply_policy
  # Detect a concurrently started Task service before publishing readiness
  # fingerprints. The service must still be systemd-ordered after this action;
  # this second check only narrows races between independent deployment tools.
  config_preflight
  ensure_task_sandbox_idle
  write_state_file config-fingerprint "$(config_fingerprint)"
  rm -f -- "$STATE_DIR/network-fingerprint"
  pass 'deny policy atomically preloaded before Task OpenSandbox starts'
}

preload() {
  require_root
  lock_policy
  make_temp_directory
  preload_policy
}

apply() {
  require_root
  lock_policy
  make_temp_directory
  config_preflight
  live_docker_runtime_preflight
  live_network_preflight
  live_task_server_preflight
  check_state_directory
  assert_preloaded_policy
  apply_policy
  config_preflight
  live_docker_runtime_preflight
  live_network_preflight
  live_task_server_preflight
  local network_json network_fingerprint
  network_json="$(docker_cli network inspect --format '{{json .}}' "$NETWORK_NAME" 2>/dev/null)" || fail 'task sandbox network disappeared after preflight'
  network_fingerprint="$(config_fingerprint "$network_json")"
  write_state_file config-fingerprint "$(config_fingerprint)"
  write_state_file network-fingerprint "$network_fingerprint"
  pass 'live task bridge, runtime, OpenSandbox service and deny policy match the protected contract'
}

prepare_network() {
  require_root
  lock_policy
  make_temp_directory
  config_preflight
  live_docker_runtime_preflight
  ensure_task_sandbox_idle
  ensure_network
  # Keep the policy lock from bridge creation through fail-closed nft preload;
  # there must not be a launch window with an unfiltered task bridge.
  preload_policy
}

check() {
  config_preflight
  live_docker_runtime_preflight
  live_network_preflight
  live_task_server_preflight
  check_state_directory
  make_temp_directory
  local expected_config expected_network expected_rules network_json actual_config actual_network actual_rules
  expected_config="$(read_state_file config-fingerprint)"
  expected_network="$(read_state_file network-fingerprint)"
  expected_rules="$(read_state_file nft-fingerprint)"
  [[ "$expected_config" =~ ^[[:xdigit:]]{64}$ && "$expected_network" =~ ^[[:xdigit:]]{64}$ && "$expected_rules" =~ ^[[:xdigit:]]{64}$ ]] || fail 'root-protected task sandbox policy state is malformed'
  network_json="$(docker_cli network inspect --format '{{json .}}' "$NETWORK_NAME" 2>/dev/null)" || fail 'task sandbox network disappeared during check'
  actual_config="$(config_fingerprint)"
  actual_network="$(config_fingerprint "$network_json")"
  actual_rules="$(table_fingerprint)"
  [[ "$actual_config" == "$expected_config" ]] || fail 'task sandbox protected configuration changed after policy load'
  [[ "$actual_network" == "$expected_network" ]] || fail 'task sandbox live Docker network changed after policy load'
  [[ "$actual_rules" == "$expected_rules" ]] || fail 'task sandbox nftables policy drifted after policy load'
  pass 'read-only check confirmed Task Sandbox config, live network and nftables policy'
}

assert_idle_locked() {
  local docker_state
  require_root
  local task_service_state
  task_service_state="$(systemctl show --property=ActiveState --value morde-task-sandbox.service 2>/dev/null)" \
    || fail 'could not inspect Task Sandbox service state before nftables changes'
  case "$task_service_state" in
    active) fail 'Task Sandbox service is active; refusing nftables lifecycle changes' ;;
    inactive|failed|"") ;;
    *) fail 'Task Sandbox service is transitioning; refusing nftables lifecycle changes' ;;
  esac
  docker_state="$(systemctl show --property=ActiveState --value docker.service 2>/dev/null)" || fail 'could not inspect Docker service state before nftables changes'
  case "$docker_state" in
    active)
      ensure_task_sandbox_idle
      ;;
    inactive|failed)
      # Do not contact a possibly socket-activated daemon before nftables has
      # completed startup. Docker will start only after its host policy is ready.
      ;;
    *)
      fail 'Docker service is transitioning; refusing nftables lifecycle changes'
      ;;
  esac
  pass 'no Task OpenSandbox service or containers are attached to the isolated task bridge'
}

assert_idle() {
  require_root
  lock_policy
  assert_idle_locked
}

main() {
  require_linux
  trap cleanup EXIT
  case "${1:-}" in
    preload)
      preload
      ;;
    apply)
      apply
      ;;
    prepare-network)
      prepare_network
      ;;
    check)
      check
      ;;
    assert-idle)
      assert_idle
      ;;
    assert-idle-locked)
      assert_idle_locked
      ;;
    *)
      fail 'usage: task-sandbox-network-policy.sh {prepare-network|preload|apply|check|assert-idle|assert-idle-locked}'
      ;;
  esac
}

main "$@"
