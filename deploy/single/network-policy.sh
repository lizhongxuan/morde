#!/usr/bin/env bash

set -Eeuo pipefail

readonly NETWORK_NAME="cloudhost"
readonly BRIDGE_NAME="chbr0"
readonly SUBNET="172.30.0.0/24"
readonly CONFIG_PATH="${CH_OSB_CONFIG_FILE:-/etc/opensandbox/opensandbox.toml}"
readonly DOCKER_CONFIG_PATH="/etc/docker/daemon.json"
readonly RESOLV_CONF_PATH="/etc/cloudhost/resolv.conf"
readonly STATE_DIR="/etc/cloudhost/network-policy"
readonly FINGERPRINT_PATH="$STATE_DIR/fingerprint"
readonly CONFIG_FINGERPRINT_PATH="$STATE_DIR/config-fingerprint"
readonly LOCK_PATH="/run/lock/cloudhost-network-policy.lock"
readonly NFT_BIN="/usr/sbin/nft"

TMP_DIR=""

fail() {
  printf 'FAIL network-policy %s\n' "$*" >&2
  exit 1
}

pass() {
  printf 'PASS network-policy %s\n' "$*"
}

cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
}

docker_cli() {
  DOCKER_HOST="${CH_DOCKER_HOST:-unix:///var/run/docker.sock}" docker "$@"
}

require_linux() {
  [[ "$(uname -s)" == Linux ]] || fail 'Linux host required'
  [[ -x "$NFT_BIN" ]] || fail 'nftables executable missing at /usr/sbin/nft'
  command -v python3 >/dev/null 2>&1 || fail 'Python 3 missing'
  python3 -c 'import tomllib' >/dev/null 2>&1 || fail 'Python 3.11 or later with tomllib is required'
  if ((EUID != 0)); then
    command -v sudo >/dev/null 2>&1 || fail 'sudo is required for read-only nftables inspection'
  fi
  command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum missing'
}

config_preflight() {
  local mode="${1:-apply}" runtimes_json="$TMP_DIR/docker-runtimes.json" network_json="$TMP_DIR/docker-network.json"

  [[ "$mode" == apply || "$mode" == check || "$mode" == preload ]] || fail 'invalid config preflight mode'

  [[ "$CONFIG_PATH" == /* && "$CONFIG_PATH" != *$'\n'* && "$CONFIG_PATH" != *$'\r'* ]] || fail 'CH_OSB_CONFIG_FILE must be an absolute single-line path'
  [[ -r "$CONFIG_PATH" ]] || fail 'OpenSandbox config is not readable'
  [[ -r "$DOCKER_CONFIG_PATH" ]] || fail 'Docker daemon config is not readable'
  if [[ "$mode" != preload ]]; then
    command -v docker >/dev/null 2>&1 || fail 'Docker CLI missing'
    docker_cli info --format '{{json .Runtimes}}' >"$runtimes_json" 2>/dev/null || fail 'Docker daemon is unavailable'
    if ! docker_cli network inspect "$NETWORK_NAME" >"$network_json" 2>/dev/null; then
      fail 'dedicated Docker network cloudhost is missing; run network-policy.sh apply as root'
    fi
  fi

  python3 - "$CONFIG_PATH" "$DOCKER_CONFIG_PATH" "$RESOLV_CONF_PATH" "$runtimes_json" "$network_json" "$mode" <<'PY'
import ipaddress
import json
import os
import stat
import sys
import tomllib
from urllib.parse import urlsplit

config_path, daemon_path, resolv_path, runtimes_path, network_path, mode = sys.argv[1:]

def protected_file(path, label):
    path = os.path.abspath(path)
    parent = os.path.dirname(path)
    while True:
        try:
            parent_info = os.lstat(parent)
        except OSError as exc:
            raise SystemExit(f"FAIL network-policy {label} parent directory is unavailable") from exc
        if not stat.S_ISDIR(parent_info.st_mode) or stat.S_ISLNK(parent_info.st_mode) or parent_info.st_uid != 0 or parent_info.st_mode & 0o022:
            raise SystemExit(f"FAIL network-policy {label} parent directories must be root-owned and not writable by group or others")
        if parent == "/":
            break
        parent = os.path.dirname(parent)
    try:
        info = os.lstat(path)
    except OSError as exc:
        raise SystemExit(f"FAIL network-policy {label} file unavailable") from exc
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise SystemExit(f"FAIL network-policy {label} must be a root-owned regular file not writable by group or others")
    if not os.access(path, os.R_OK):
        raise SystemExit(f"FAIL network-policy {label} is not readable by the Portal process")

protected_file(config_path, "OpenSandbox config")
protected_file(daemon_path, "Docker daemon config")
protected_file(resolv_path, "sandbox resolver config")
try:
    with open(config_path, "rb") as source:
        config = tomllib.load(source)
    with open(daemon_path, "r", encoding="utf-8") as source:
        daemon = json.load(source)
    with open(resolv_path, encoding="utf-8") as source:
        resolv_lines = source.read().splitlines()
    if mode != "preload":
        with open(runtimes_path, encoding="utf-8") as source:
            runtimes = json.load(source)
        with open(network_path, encoding="utf-8") as source:
            networks = json.load(source)
except (OSError, json.JSONDecodeError, tomllib.TOMLDecodeError) as exc:
    raise SystemExit("FAIL network-policy deployment config could not be parsed") from exc

server = config.get("server", {})
runtime = config.get("runtime", {})
docker = config.get("docker", {})
secure = config.get("secure_runtime", {})
if server.get("host") != "127.0.0.1" or server.get("eip") != "127.0.0.1":
    raise SystemExit("FAIL network-policy OpenSandbox endpoint EIP must be 127.0.0.1")
server_port = server.get("port")
if type(server_port) is not int or not 1 <= server_port <= 65535:
    raise SystemExit("FAIL network-policy OpenSandbox server port is invalid")
if mode == "check":
    base_url = os.environ.get("CH_OSB_URL", "")
    try:
        endpoint = urlsplit(base_url)
        endpoint_port = endpoint.port
    except ValueError as exc:
        raise SystemExit("FAIL network-policy Portal OpenSandbox URL is invalid") from exc
    if (endpoint.scheme != "http" or endpoint.hostname != "127.0.0.1" or endpoint.username is not None
            or endpoint.password is not None or endpoint_port != server_port or endpoint.path not in ("", "/")
            or endpoint.query or endpoint.fragment or "?" in base_url or "#" in base_url):
        raise SystemExit("FAIL network-policy Portal OpenSandbox URL must bind to the protected loopback server")
if runtime.get("type") != "docker" or secure.get("type") != "gvisor" or secure.get("docker_runtime") != "runsc":
    raise SystemExit("FAIL network-policy OpenSandbox must use Docker with gVisor runsc")
if docker.get("network_mode") != "cloudhost" or docker.get("publish_host") != "127.0.0.1":
    raise SystemExit("FAIL network-policy OpenSandbox must use cloudhost with loopback published ports")
if docker.get("sandbox_binds") != ["/etc/cloudhost/resolv.conf:/etc/resolv.conf:ro"]:
    raise SystemExit("FAIL network-policy OpenSandbox sandbox_binds must mount the protected resolver read-only")
if resolv_lines != ["nameserver 1.1.1.1", "nameserver 8.8.8.8"]:
    raise SystemExit("FAIL network-policy protected sandbox resolver must contain only the approved public nameservers")
drop_capabilities = docker.get("drop_capabilities")
if not isinstance(drop_capabilities, list) or "NET_RAW" not in drop_capabilities:
    raise SystemExit("FAIL network-policy OpenSandbox must drop NET_RAW")
if "egress" in config:
    raise SystemExit("FAIL network-policy OpenSandbox egress sidecar config must be absent")

runtime_args = daemon.get("runtimes", {}).get("runsc", {}).get("runtimeArgs")
if not isinstance(runtime_args, list):
    raise SystemExit("FAIL network-policy Docker runsc runtimeArgs are missing")
for flag in ("--net-raw", "--allow-packet-socket-write"):
    values = [arg for arg in runtime_args if arg == flag or (isinstance(arg, str) and arg.startswith(flag + "="))]
    if values != [flag + "=false"]:
        raise SystemExit(f"FAIL network-policy Docker runsc must set {flag}=false exactly once")
if mode == "preload":
    print("PASS network-policy protected static configuration permits early nftables preload")
    raise SystemExit(0)
if not isinstance(runtimes, dict) or "runsc" not in runtimes:
    raise SystemExit("FAIL network-policy Docker has not registered the runsc runtime")
live_runtime_args = runtimes["runsc"].get("runtimeArgs")
if not isinstance(live_runtime_args, list) or live_runtime_args != runtime_args:
    raise SystemExit("FAIL network-policy live Docker runsc runtimeArgs differ from the protected daemon config")
for flag in ("--net-raw", "--allow-packet-socket-write"):
    values = [arg for arg in live_runtime_args if arg == flag or (isinstance(arg, str) and arg.startswith(flag + "="))]
    if values != [flag + "=false"]:
        raise SystemExit(f"FAIL network-policy live Docker runsc must set {flag}=false exactly once")

if not isinstance(networks, list) or len(networks) != 1:
    raise SystemExit("FAIL network-policy Docker cloudhost network inspection was ambiguous")
network = networks[0]
options = network.get("Options") or {}
subnets = [item.get("Subnet") for item in (network.get("IPAM") or {}).get("Config") or []]
if network.get("Driver") != "bridge" or network.get("EnableIPv6") is not False:
    raise SystemExit("FAIL network-policy cloudhost must be an IPv4-only Docker bridge")
if subnets != ["172.30.0.0/24"]:
    raise SystemExit("FAIL network-policy cloudhost subnet must be 172.30.0.0/24")
if options.get("com.docker.network.bridge.name") != "chbr0":
    raise SystemExit("FAIL network-policy cloudhost bridge name must be chbr0")
if options.get("com.docker.network.bridge.enable_icc") != "false":
    raise SystemExit("FAIL network-policy cloudhost inter-container connectivity must be disabled")
if options.get("com.docker.network.bridge.enable_ip_masquerade") != "true":
    raise SystemExit("FAIL network-policy cloudhost IPv4 masquerading must be enabled")
if options.get("com.docker.network.bridge.host_binding_ipv4") != "127.0.0.1":
    raise SystemExit("FAIL network-policy cloudhost default published-port binding must be loopback")
gateway = ((network.get("IPAM") or {}).get("Config") or [{}])[0].get("Gateway")
if gateway != "172.30.0.1" or ipaddress.ip_network(subnets[0]).version != 4:
    raise SystemExit("FAIL network-policy cloudhost gateway or address family is invalid")

print("PASS network-policy root-owned OpenSandbox and Docker configs enforce runsc, NET_RAW denial, and loopback publishing")
if mode == "check":
    print("PASS network-policy Portal OpenSandbox URL matches the protected loopback server")
print("PASS network-policy Docker cloudhost bridge settings match the isolated IPv4 network")
PY
}

server_container_preflight() {
  local ids_file="$TMP_DIR/osb-container-ids" mounts_file="$TMP_DIR/osb-mounts.json"
  local network_mode_file="$TMP_DIR/osb-network-mode" started_at_file="$TMP_DIR/osb-started-at"
  local executable_file="$TMP_DIR/osb-executable" args_match
  docker_cli ps --quiet \
    --filter 'label=com.docker.compose.project=cloudhost' \
    --filter 'label=com.docker.compose.service=opensandbox-server' >"$ids_file" 2>/dev/null \
    || fail 'could not inspect the OpenSandbox Compose service'
  [[ "$(wc -l <"$ids_file" | tr -d ' ')" == 1 ]] || fail 'exactly one running cloudhost OpenSandbox server is required'
  local container_id
  IFS= read -r container_id <"$ids_file"
  [[ "$container_id" =~ ^[[:xdigit:]]{12,64}$ ]] || fail 'OpenSandbox container identifier was invalid'
  docker_cli inspect --format '{{json .Mounts}}' "$container_id" >"$mounts_file" 2>/dev/null || fail 'could not inspect OpenSandbox mounts'
  docker_cli inspect --format '{{.HostConfig.NetworkMode}}' "$container_id" >"$network_mode_file" 2>/dev/null || fail 'could not inspect OpenSandbox network mode'
  docker_cli inspect --format '{{.State.StartedAt}}' "$container_id" >"$started_at_file" 2>/dev/null || fail 'could not inspect OpenSandbox start time'
  docker_cli inspect --format '{{.Path}}' "$container_id" >"$executable_file" 2>/dev/null || fail 'could not inspect the OpenSandbox executable'
  args_match="$(docker_cli inspect --format '{{if eq (len .Args) 2}}{{if and (eq (index .Args 0) "--config") (eq (index .Args 1) "/etc/opensandbox/opensandbox.toml")}}true{{end}}{{end}}' "$container_id" 2>/dev/null)" \
    || fail 'could not verify the OpenSandbox config arguments'

  python3 - "$CONFIG_PATH" "$mounts_file" "$network_mode_file" "$started_at_file" "$executable_file" "$args_match" <<'PY'
import datetime
import json
import os
import sys

config_path, mounts_path, network_mode_path, started_at_path, executable_path, args_match = sys.argv[1:]
try:
    with open(mounts_path, encoding="utf-8") as source:
        mounts = json.load(source)
    with open(network_mode_path, encoding="utf-8") as source:
        network_mode = source.read().strip()
    with open(started_at_path, encoding="utf-8") as source:
        started_at_value = source.read().strip()
    with open(executable_path, encoding="utf-8") as source:
        executable = source.read().strip()
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL network-policy OpenSandbox sanitized inspection was invalid") from exc
target = "/etc/opensandbox/opensandbox.toml"
if not isinstance(mounts, list):
    raise SystemExit("FAIL network-policy OpenSandbox mount inspection was empty")
mounts = [mount for mount in mounts if mount.get("Destination") == target]
expected_source = os.path.abspath(config_path)
if len(mounts) != 1 or os.path.abspath(mounts[0].get("Source", "")) != expected_source or mounts[0].get("RW") is not False:
    raise SystemExit("FAIL network-policy running OpenSandbox must mount the protected config read-only")
if network_mode != "host":
    raise SystemExit("FAIL network-policy OpenSandbox server must retain host networking")
if os.path.basename(executable) != "opensandbox-server" or args_match != "true":
    raise SystemExit("FAIL network-policy running OpenSandbox must execute opensandbox-server --config with the protected config path")
try:
    started_at = datetime.datetime.fromisoformat(started_at_value.replace("Z", "+00:00"))
    mtime_ns = os.stat(config_path, follow_symlinks=False).st_mtime_ns
except (KeyError, OSError, ValueError) as exc:
    raise SystemExit("FAIL network-policy OpenSandbox start time or config timestamp was unavailable") from exc
if mtime_ns > int(started_at.timestamp() * 1_000_000_000):
    raise SystemExit("FAIL network-policy OpenSandbox config changed after the server started")
print("PASS network-policy running OpenSandbox executes with the protected read-only config mounted before startup")
PY
}

nft_table() {
  local family="$1"
  if ((EUID == 0)); then
    "$NFT_BIN" -j -s -nn list table "$family" cloudhost
  else
    sudo -n "$NFT_BIN" -j -s -nn list table "$family" cloudhost
  fi
}

current_fingerprint() {
  local inet_table bridge_table
  inet_table="$(nft_table inet)" || return 1
  bridge_table="$(nft_table bridge)" || return 1
  python3 - "$inet_table" "$bridge_table" <<'PY' | sha256sum | awk '{print $1}'
import json
import sys

for raw in sys.argv[1:]:
    table = json.loads(raw)
    for item in table.get("nftables", []):
        value = item.get("set")
        if value and value.get("name") == "relay_enabled_ifaces":
            value.pop("elem", None)
    print(json.dumps(table, sort_keys=True, separators=(",", ":")))
PY
}

workspace_sync_relay_bind_ipv4() {
  python3 - "${CH_WORKSPACE_SYNC_RELAY_PUBLIC_ADDRESS:-}" "${CH_WORKSPACE_SYNC_RELAY_BIND_ADDRESS:-}" <<'PY'
import ipaddress
import sys

public_value, bind_value = sys.argv[1:]
if not public_value or public_value == "127.0.0.1:22067":
    print("")
    raise SystemExit(0)
host, separator, port = public_value.rpartition(":")
if not separator or port != "22067":
    raise SystemExit("FAIL network-policy Workspace relay must be a public IPv4 address on TCP 22067")
try:
    public = ipaddress.ip_address(host)
except ValueError as exc:
    raise SystemExit("FAIL network-policy Workspace relay must use a static public IPv4 address") from exc
if public.version != 4 or not public.is_global:
    raise SystemExit("FAIL network-policy Workspace relay must use a static public IPv4 address")
try:
    bind = ipaddress.ip_address(bind_value)
except ValueError as exc:
    raise SystemExit("FAIL network-policy Workspace relay bind must be a usable host IPv4 address") from exc
if bind.version != 4 or bind.is_unspecified or bind.is_loopback or bind.is_link_local or bind.is_multicast:
    raise SystemExit("FAIL network-policy Workspace relay bind must be a usable host IPv4 address")
print(bind)
PY
}

workspace_sync_config_fingerprint() {
  printf '%s\0%s\0' "${CH_WORKSPACE_SYNC_RELAY_PUBLIC_ADDRESS:-}" "${CH_WORKSPACE_SYNC_RELAY_BIND_ADDRESS:-}" \
    | sha256sum | awk '{print $1}'
}

stop_workspace_sync_nodes() {
  local running_file="$TMP_DIR/workspace-sync-running.txt" remaining_file="$TMP_DIR/workspace-sync-remaining.txt"
  local container_id docker_state
  local -a container_ids=()

  command -v systemctl >/dev/null 2>&1 || return 1
  docker_state="$(systemctl show --property=ActiveState --value docker.service 2>/dev/null)" || return 1
  [[ "$docker_state" == inactive ]] && return 0
  [[ "$docker_state" == active ]] || return 1
  command -v docker >/dev/null 2>&1 || return 1
  docker_cli ps --quiet --filter 'label=morde.workspace-sync.managed=true' >"$running_file" 2>/dev/null || return 1
  while IFS= read -r container_id; do
    [[ -n "$container_id" ]] || continue
    [[ "$container_id" =~ ^[[:xdigit:]]{12,64}$ ]] || return 1
    container_ids+=("$container_id")
  done <"$running_file"
  if ((${#container_ids[@]})); then
    docker_cli stop --time 20 "${container_ids[@]}" >/dev/null 2>&1 || return 1
  fi
  docker_cli ps --quiet --filter 'label=morde.workspace-sync.managed=true' >"$remaining_file" 2>/dev/null || return 1
  [[ ! -s "$remaining_file" ]]
}

portal_service_stopped() {
  local load_state active_state main_pid control_pid
  command -v systemctl >/dev/null 2>&1 || return 1
  load_state="$(systemctl show --property=LoadState --value morde-portal.service 2>/dev/null)" || return 1
  [[ "$load_state" == not-found ]] && return 0
  active_state="$(systemctl show --property=ActiveState --value morde-portal.service 2>/dev/null)" || return 1
  [[ "$active_state" == inactive ]] && return 0
  [[ "$active_state" == failed ]] || return 1
  main_pid="$(systemctl show --property=MainPID --value morde-portal.service 2>/dev/null)" || return 1
  control_pid="$(systemctl show --property=ControlPID --value morde-portal.service 2>/dev/null)" || return 1
  [[ "$main_pid" == 0 && "$control_pid" == 0 ]]
}

stop_portal_service() {
  if ! portal_service_stopped; then
    systemctl stop morde-portal.service >/dev/null 2>&1 || {
      systemctl kill --kill-who=all --signal=SIGKILL morde-portal.service >/dev/null 2>&1 || true
      systemctl stop morde-portal.service >/dev/null 2>&1 || true
    }
  fi
  portal_service_stopped
}

clear_workspace_sync_relay_gates() {
  local command_file="$TMP_DIR/relay-disable-all.nft"
  printf 'flush set inet cloudhost relay_enabled_ifaces\n' >"$command_file"
  "$NFT_BIN" --check --file "$command_file" >/dev/null 2>&1 || return 1
  "$NFT_BIN" --file "$command_file" >/dev/null 2>&1
}

close_relay_gates_or_stop_nodes() {
  clear_workspace_sync_relay_gates || stop_workspace_sync_nodes
}

fail_closed_state_drift() {
  local reason="$1"
  if ((EUID == 0)); then
    close_relay_gates_or_stop_nodes || fail "$reason; could not close relay gates or stop Workspace sync nodes"
  else
    sudo -n /opt/cloudhost/network-policy.sh relay-disable-all >/dev/null 2>&1 \
      || fail "$reason; could not revoke Workspace sync relay access"
  fi
  fail "$reason; relay access was revoked, apply the current network policy before starting Portal"
}

check_protected_directory_chain() {
  local target="$1" current="/" part owner mode
  local -a parts
  [[ "$target" == /* ]] || fail 'network-policy protected directory path must be absolute'
  IFS='/' read -r -a parts <<<"$target"
  for part in "${parts[@]}"; do
    [[ -n "$part" ]] || continue
    current="${current%/}/$part"
    [[ -d "$current" && ! -L "$current" ]] || fail 'network-policy state directory ancestry must contain only real directories'
    owner="$(stat -c '%u' "$current" 2>/dev/null)" || fail 'cannot inspect network-policy state directory ownership'
    [[ "$owner" == 0 ]] || fail 'network-policy state directory ancestry must be root-owned'
    mode="$(stat -c '%a' "$current" 2>/dev/null)" || fail 'cannot inspect network-policy state directory permissions'
    (( (8#$mode & 8#022) == 0 )) || fail 'network-policy state directory ancestry is writable by group or others'
  done
}

check_root_state() {
  local verify_relay_config="${1:-yes}"
  [[ "$verify_relay_config" == yes || "$verify_relay_config" == rules-only ]] || fail 'invalid network-policy check mode'
  check_protected_directory_chain "$STATE_DIR"
  [[ -f "$FINGERPRINT_PATH" && ! -L "$FINGERPRINT_PATH" ]] || fail_closed_state_drift 'root-protected nftables fingerprint is missing'
  [[ "$(stat -c '%u' "$FINGERPRINT_PATH" 2>/dev/null)" == 0 ]] || fail_closed_state_drift 'nftables fingerprint must be root-owned'
  local file_mode expected actual
  file_mode="$(stat -c '%a' "$FINGERPRINT_PATH" 2>/dev/null)" || fail 'cannot inspect nftables fingerprint permissions'
  (( (8#$file_mode & 8#022) == 0 )) || fail_closed_state_drift 'nftables fingerprint is writable by group or others'
  expected="$(cat -- "$FINGERPRINT_PATH")" || fail 'cannot read the root-protected nftables fingerprint'
  [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || fail_closed_state_drift 'nftables fingerprint format is invalid'
  actual="$(current_fingerprint)" || fail_closed_state_drift 'dedicated nftables tables cannot be read'
  [[ "$actual" == "$expected" ]] || fail_closed_state_drift 'dedicated nftables rules differ from the root-applied fingerprint'

  if [[ "$verify_relay_config" == rules-only ]]; then
    pass 'dedicated nftables tables match the root-applied policy'
    return
  fi

  [[ -f "$CONFIG_FINGERPRINT_PATH" && ! -L "$CONFIG_FINGERPRINT_PATH" ]] || fail_closed_state_drift 'root-protected relay configuration fingerprint is missing'
  [[ "$(stat -c '%u' "$CONFIG_FINGERPRINT_PATH" 2>/dev/null)" == 0 ]] || fail_closed_state_drift 'relay configuration fingerprint must be root-owned'
  file_mode="$(stat -c '%a' "$CONFIG_FINGERPRINT_PATH" 2>/dev/null)" || fail_closed_state_drift 'cannot inspect relay configuration fingerprint permissions'
  (( (8#$file_mode & 8#022) == 0 )) || fail_closed_state_drift 'relay configuration fingerprint is writable by group or others'
  expected="$(cat -- "$CONFIG_FINGERPRINT_PATH")" || fail_closed_state_drift 'cannot read the root-protected relay configuration fingerprint'
  [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || fail_closed_state_drift 'relay configuration fingerprint format is invalid'
  actual="$(workspace_sync_config_fingerprint)"
  [[ "$actual" == "$expected" ]] || fail_closed_state_drift 'configured Workspace relay address differs from the root-applied nftables policy'
  pass 'dedicated nftables tables and Workspace relay configuration match the root-applied policy'
}

lock_policy_state() {
  local prepare_state="${1:-yes}"
  ((EUID == 0)) || fail 'network policy changes must run as root'
  command -v flock >/dev/null 2>&1 || fail 'flock is required for network policy changes'
  [[ "$prepare_state" == yes || "$prepare_state" == no ]] || fail 'invalid network-policy state mode'
  if [[ "$prepare_state" == yes ]]; then
    check_protected_directory_chain "$(dirname "$STATE_DIR")"
    if [[ -e "$STATE_DIR" || -L "$STATE_DIR" ]]; then
      [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || fail 'network-policy state path must be a real directory'
    else
      install -d -o root -g root -m 0755 "$STATE_DIR"
    fi
    chown root:root "$STATE_DIR"
    chmod 0755 "$STATE_DIR"
  else
    [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || fail 'network-policy state path must be a real directory'
  fi
  check_protected_directory_chain "$STATE_DIR"
  [[ ! -L "$LOCK_PATH" ]] || fail 'network-policy lock path must not be a symlink'
  if [[ ! -e "$LOCK_PATH" ]]; then
    install -o root -g root -m 0600 /dev/null "$LOCK_PATH"
  fi
  [[ "$(stat -c '%u' "$LOCK_PATH" 2>/dev/null)" == 0 ]] || fail 'network-policy lock must be root-owned'
  local lock_mode
  lock_mode="$(stat -c '%a' "$LOCK_PATH" 2>/dev/null)" || fail 'cannot inspect network-policy lock permissions'
  (( (8#$lock_mode & 8#022) == 0 )) || fail 'network-policy lock is writable by group or others'
  exec 9>"$LOCK_PATH"
  flock -x 9
}

build_policy_rules() {
  local relay_ip
  relay_ip="$(workspace_sync_relay_bind_ipv4)" || fail 'Workspace relay bind destination is invalid'
  cat >"$TMP_DIR/rules.nft" <<'NFT'
destroy table inet cloudhost
destroy table bridge cloudhost
add table inet cloudhost
add set inet cloudhost blocked4 { type ipv4_addr; flags interval; elements = { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.0.2.0/24, 192.88.99.0/24, 192.168.0.0/16, 198.18.0.0/15, 198.51.100.0/24, 203.0.113.0/24, 224.0.0.0/4, 240.0.0.0/4 } }
add set inet cloudhost relay_enabled_ifaces { type ifname; }
add chain inet cloudhost input { type filter hook input priority filter - 1; policy accept; }
add chain inet cloudhost forward { type filter hook forward priority filter - 1; policy accept; }
add rule inet cloudhost input iifname "chbr0" ct state { established, related } accept
add rule inet cloudhost input iifname "chbr0" counter drop
add rule inet cloudhost input iifname "msync*" meta l4proto tcp ct state established ct original proto-dst 8384 accept
add rule inet cloudhost input iifname "msync*" counter drop
add rule inet cloudhost forward iifname "msync*" meta nfproto ipv6 counter drop
__WORKSPACE_SYNC_RELAY_ALLOW__
add rule inet cloudhost forward iifname "msync*" counter drop
__WORKSPACE_SYNC_RELAY_RETURN__
add rule inet cloudhost forward oifname "msync*" counter drop
add rule inet cloudhost forward iifname "chbr0" ct direction original ip daddr @blocked4 counter drop
add rule inet cloudhost forward iifname "chbr0" meta nfproto ipv6 counter drop
add table bridge cloudhost
add chain bridge cloudhost forward { type filter hook forward priority filter; policy accept; }
add rule bridge cloudhost forward ibrname "chbr0" obrname "chbr0" counter drop
NFT

  python3 - "$TMP_DIR/rules.nft" "$relay_ip" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
rules = path.read_text()
allow_marker = "__WORKSPACE_SYNC_RELAY_ALLOW__"
return_marker = "__WORKSPACE_SYNC_RELAY_RETURN__"
if rules.count(allow_marker) != 1 or rules.count(return_marker) != 1:
    raise SystemExit("FAIL network-policy Workspace relay rule markers are invalid")
ip = sys.argv[2]
allow_rule = (
    f'add rule inet cloudhost forward iifname "msync*" iifname @relay_enabled_ifaces meta l4proto tcp '
    f'ct original ip daddr {ip} ct original proto-dst 22067 accept'
) if ip else ""
return_rule = (
    f'add rule inet cloudhost forward oifname "msync*" oifname @relay_enabled_ifaces '
    f'ct state {{ established, related }} ct original ip daddr {ip} ct original proto-dst 22067 accept'
) if ip else ""
path.write_text(rules.replace(allow_marker, allow_rule).replace(return_marker, return_rule))
PY
}

write_policy_fingerprints() {
  local fingerprint config_fingerprint temp_fingerprint
  fingerprint="$(current_fingerprint)" || fail 'could not read the applied nftables tables'
  [[ "$fingerprint" =~ ^[[:xdigit:]]{64}$ ]] || fail 'applied nftables fingerprint was invalid'
  temp_fingerprint="$(mktemp "$STATE_DIR/.fingerprint.XXXXXX")"
  printf '%s\n' "$fingerprint" >"$temp_fingerprint"
  chown root:root "$temp_fingerprint"
  chmod 0644 "$temp_fingerprint"
  mv -f -- "$temp_fingerprint" "$FINGERPRINT_PATH"

  config_fingerprint="$(workspace_sync_config_fingerprint)"
  temp_fingerprint="$(mktemp "$STATE_DIR/.config-fingerprint.XXXXXX")"
  printf '%s\n' "$config_fingerprint" >"$temp_fingerprint"
  chown root:root "$temp_fingerprint"
  chmod 0644 "$temp_fingerprint"
  mv -f -- "$temp_fingerprint" "$CONFIG_FINGERPRINT_PATH"
}

apply_policy_rules() {
  build_policy_rules
  "$NFT_BIN" --check --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables rejected the isolated network policy; no rules were changed'
  "$NFT_BIN" --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables could not atomically replace the dedicated network-policy tables'
  write_policy_fingerprints
}

preload_network() {
  lock_policy_state
  # Docker must not be contacted here: its dependency waits for this service.
  config_preflight preload
  apply_policy_rules
  pass 'nftables isolation policy preloaded before Docker starts'
}

nftables_before() {
  ((EUID == 0)) || fail 'nftables lifecycle actions must run as root'
  lock_policy_state
  portal_service_stopped || fail 'Portal must be stopped before loading the nftables base configuration'
  close_relay_gates_or_stop_nodes || fail 'could not revoke relay access or stop Workspace sync nodes before nftables start'
  stop_workspace_sync_nodes || {
    close_relay_gates_or_stop_nodes || true
    fail 'Workspace sync nodes could not be stopped before nftables start'
  }
  pass 'Portal and managed Workspace sync nodes are stopped before nftables loads its base configuration'
}

nftables_stop() {
  ((EUID == 0)) || fail 'nftables lifecycle actions must run as root'
  lock_policy_state
  if ! stop_portal_service; then
    stop_workspace_sync_nodes || true
    close_relay_gates_or_stop_nodes || true
    fail 'Portal could not be stopped; nftables ruleset was preserved'
  fi
  if ! stop_workspace_sync_nodes; then
    close_relay_gates_or_stop_nodes || true
    fail 'Workspace sync nodes could not be stopped; nftables ruleset was preserved'
  fi
  clear_workspace_sync_relay_gates || fail 'relay gates could not be cleared; the Morde deny policy was preserved'
  pass 'Portal and managed Workspace sync nodes stopped; Morde deny policy remains loaded'
}

nftables_reload() {
  ((EUID == 0)) || fail 'nftables lifecycle actions must run as root'
  local portal_state portal_was_active=no base_config="$TMP_DIR/nftables.conf" combined="$TMP_DIR/nftables-with-morde.nft"
  portal_state="$(systemctl show --property=ActiveState --value morde-portal.service 2>/dev/null)" \
    || fail 'could not inspect Portal before nftables reload'
  case "$portal_state" in
    active) portal_was_active=yes ;;
    inactive|failed|"") ;;
    *) fail "Portal is $portal_state; refusing nftables reload during a unit transition" ;;
  esac
  if ! stop_portal_service; then
    stop_workspace_sync_nodes || true
    close_relay_gates_or_stop_nodes || true
    fail 'Portal could not be stopped; nftables configuration was not reloaded'
  fi

  lock_policy_state
  if ! stop_workspace_sync_nodes; then
    close_relay_gates_or_stop_nodes || true
    fail 'Workspace sync nodes could not be stopped; nftables configuration was not reloaded'
  fi
  close_relay_gates_or_stop_nodes || fail 'could not establish a closed relay gate before nftables reload'

  [[ -f /etc/nftables.conf && ! -L /etc/nftables.conf && $(stat -c '%u' /etc/nftables.conf) == 0 ]] \
    || fail '/etc/nftables.conf must be a root-owned regular file'
  config_preflight apply
  build_policy_rules
  cat -- /etc/nftables.conf >"$base_config" || fail 'could not stage the nftables base configuration'
  printf '\n' >>"$base_config"
  cat -- "$base_config" "$TMP_DIR/rules.nft" >"$combined" || fail 'could not combine base and Morde nftables policy'
  "$NFT_BIN" --check --file "$combined" >/dev/null \
    || fail 'nftables rejected the combined base and Morde policy; Portal and sync nodes remain stopped'
  "$NFT_BIN" --file "$combined" >/dev/null \
    || fail 'nftables could not atomically apply the combined base and Morde policy; Portal and sync nodes remain stopped'
  write_policy_fingerprints

  if [[ "$portal_was_active" == yes ]]; then
    systemctl start --no-block morde-portal.service \
      || fail 'nftables policy was applied but Portal restart could not be queued; relay gates remain closed'
  fi
  pass 'nftables base and Morde policy applied atomically; Portal recovery was queued when it had been active'
}

workspace_sync_owner_hash_from_stdin() {
  local value extra
  IFS= read -r value || fail 'relay gate owner hash is required on stdin'
  [[ "$value" =~ ^[a-f0-9]{64}$ ]] || fail 'relay gate owner hash must be lowercase SHA-256 hex'
  if IFS= read -r extra; then
    fail 'relay gate accepts exactly one owner hash'
  fi
  printf '%s' "$value"
}

workspace_sync_gate_set_contains() {
  local interface="$1" output_file="$TMP_DIR/relay-gate-set.json"
  "$NFT_BIN" -j -s list set inet cloudhost relay_enabled_ifaces >"$output_file" 2>/dev/null || fail 'relay gate nftables set is unavailable'
  python3 - "$output_file" "$interface" <<'PY'
import json
import sys

path, interface = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        document = json.load(source)
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL network-policy relay gate set inspection was invalid") from exc

def contains(value):
    if isinstance(value, dict):
        if value.get("val") == interface:
            return True
        return any(contains(item) for item in value.values())
    if isinstance(value, list):
        return any(contains(item) for item in value)
    return value == interface

for item in document.get("nftables", []):
    value = item.get("set")
    if value and value.get("name") == "relay_enabled_ifaces":
        print("yes" if contains(value.get("elem", [])) else "no")
        raise SystemExit(0)
raise SystemExit("FAIL network-policy relay gate set is missing")
PY
}

validate_workspace_sync_network_for_gate() {
  local owner_hash="$1" network_file="$TMP_DIR/relay-gate-network.json"
  local network_name="morde-sync-net-${owner_hash:0:32}"
  local bridge_name="msync${owner_hash:0:10}"
  docker_cli network inspect "$network_name" >"$network_file" 2>/dev/null || fail 'owner Workspace sync bridge is unavailable'
  python3 - "$network_file" "$owner_hash" "$network_name" "$bridge_name" <<'PY'
import json
import sys

path, owner_hash, network_name, bridge_name = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as source:
        values = json.load(source)
except (OSError, json.JSONDecodeError) as exc:
    raise SystemExit("FAIL network-policy owner bridge inspection was invalid") from exc
if not isinstance(values, list) or len(values) != 1:
    raise SystemExit("FAIL network-policy owner bridge inspection was ambiguous")
network = values[0]
expected_labels = {"morde.workspace-sync.managed": "true", "morde.workspace-sync.owner-sha256": owner_hash}
expected_options = {
    "com.docker.network.bridge.name": bridge_name,
    "com.docker.network.bridge.enable_ip_masquerade": "true",
}
containers = network.get("Containers") or {}
members = [item.get("Name") for item in containers.values() if isinstance(item, dict)]
if (network.get("Name") != network_name or network.get("Driver") != "bridge" or network.get("Internal") is not False
        or network.get("EnableIPv6") is not False or network.get("Attachable") is not False
        or network.get("Ingress") is not False or network.get("ConfigOnly") is not False
        or network.get("Labels") != expected_labels or network.get("Options") != expected_options
        or len(containers) != 1 or members != ["morde-sync-" + owner_hash[:32]]):
    raise SystemExit("FAIL network-policy owner bridge does not match its managed single-node isolation contract")
print(bridge_name)
PY
}

change_workspace_sync_relay_gate() {
  local action="$1" owner_hash interface enabled command_file
  ((EUID == 0)) || fail 'relay gate changes must run as root'
  [[ "$#" == 1 ]] || fail 'relay gate action takes one owner hash on stdin'
  owner_hash="$(workspace_sync_owner_hash_from_stdin)"
  lock_policy_state no
  TMP_DIR="$(mktemp -d /run/cloudhost-network-gate.XXXXXX)"
  chmod 0700 "$TMP_DIR"
  trap cleanup EXIT

  interface="msync${owner_hash:0:10}"
  if [[ "$action" == relay-enable-stdin ]]; then
    check_root_state rules-only
    interface="$(validate_workspace_sync_network_for_gate "$owner_hash")"
    command -v ip >/dev/null 2>&1 || fail 'iproute2 is required to enable a Workspace sync relay route'
    ip -o link show dev "$interface" >/dev/null 2>&1 || fail 'owner Workspace sync bridge interface is missing'
  else
    [[ "$action" == relay-disable-stdin ]] || fail 'invalid relay gate action'
  fi

  enabled="$(workspace_sync_gate_set_contains "$interface")"
  if [[ "$action" == relay-enable-stdin && "$enabled" == no || "$action" == relay-disable-stdin && "$enabled" == yes ]]; then
    command_file="$TMP_DIR/relay-gate.nft"
    if [[ "$action" == relay-enable-stdin ]]; then
      printf 'add element inet cloudhost relay_enabled_ifaces { "%s" }\n' "$interface" >"$command_file"
    else
      printf 'delete element inet cloudhost relay_enabled_ifaces { "%s" }\n' "$interface" >"$command_file"
    fi
    "$NFT_BIN" --check --file "$command_file" >/dev/null || fail 'nftables rejected the relay gate change'
    "$NFT_BIN" --file "$command_file" >/dev/null || fail 'nftables could not change the relay gate'
  fi
  if [[ "$action" == relay-disable-stdin ]]; then
    check_root_state rules-only
  fi
  if [[ "$action" == relay-enable-stdin ]]; then
    pass "Workspace sync relay enabled for owner bridge $interface"
  else
    pass "Workspace sync relay disabled for owner bridge $interface"
  fi
}

disable_all_workspace_sync_relay_gates() {
  ((EUID == 0)) || fail 'relay gate changes must run as root'
  [[ "$#" == 1 ]] || fail 'relay-disable-all takes no arguments'
  lock_policy_state
  TMP_DIR="$(mktemp -d /run/cloudhost-network-gate.XXXXXX)"
  chmod 0700 "$TMP_DIR"
  trap cleanup EXIT
  close_relay_gates_or_stop_nodes || fail 'could not close relay gates or stop Workspace sync nodes'
  pass 'all Workspace sync relay gates disabled'
}

apply_network() {
  lock_policy_state
  command -v docker >/dev/null 2>&1 || fail 'Docker CLI missing'
  if ! docker_cli network inspect "$NETWORK_NAME" >/dev/null 2>&1; then
    docker_cli network create --driver bridge --ipv6=false --subnet "$SUBNET" \
      --opt "com.docker.network.bridge.name=$BRIDGE_NAME" \
      --opt com.docker.network.bridge.enable_icc=false \
      --opt com.docker.network.bridge.enable_ip_masquerade=true \
      --opt com.docker.network.bridge.host_binding_ipv4=127.0.0.1 \
      "$NETWORK_NAME" >/dev/null || fail 'could not create the dedicated cloudhost Docker network'
  fi

  config_preflight apply
  apply_policy_rules
  pass 'dedicated cloudhost network and atomic nftables isolation policy applied'
}

main() {
  local action="${1:-}"
  if [[ "$action" == relay-disable-all ]]; then
    require_linux
    disable_all_workspace_sync_relay_gates "$@"
    return
  fi
  if [[ "$action" == relay-enable-stdin || "$action" == relay-disable-stdin ]]; then
    [[ "$#" == 1 ]] || fail 'relay gate action takes one owner hash on stdin'
    require_linux
    change_workspace_sync_relay_gate "$action"
    return
  fi
  [[ "$#" == 1 ]] || fail 'usage: network-policy.sh check|preload|apply|relay-enable-stdin|relay-disable-stdin|relay-disable-all|nftables-before|nftables-stop|nftables-reload'
  case "$action" in
    check)
      require_linux
      TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cloudhost-network-check.XXXXXX")"
      chmod 0700 "$TMP_DIR"
      trap cleanup EXIT
      if ((EUID == 0)); then
        lock_policy_state no
      fi
      check_root_state
      config_preflight check
      server_container_preflight
      ;;
    apply)
      require_linux
      TMP_DIR="$(mktemp -d /run/cloudhost-network-apply.XXXXXX)"
      chmod 0700 "$TMP_DIR"
      trap cleanup EXIT
      apply_network
      ;;
    preload)
      require_linux
      TMP_DIR="$(mktemp -d /run/cloudhost-network-preload.XXXXXX)"
      chmod 0700 "$TMP_DIR"
      trap cleanup EXIT
      preload_network
      ;;
    nftables-before|nftables-stop|nftables-reload)
      require_linux
      TMP_DIR="$(mktemp -d /run/cloudhost-network-nftables.XXXXXX)"
      chmod 0700 "$TMP_DIR"
      trap cleanup EXIT
      case "$action" in
        nftables-before) nftables_before ;;
        nftables-stop) nftables_stop ;;
        nftables-reload) nftables_reload ;;
      esac
      ;;
    *) fail 'usage: network-policy.sh check|preload|apply|relay-enable-stdin|relay-disable-stdin|relay-disable-all|nftables-before|nftables-stop|nftables-reload' ;;
  esac
}

main "$@"
