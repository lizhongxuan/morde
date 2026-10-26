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
    docker info --format '{{json .Runtimes}}' >"$runtimes_json" 2>/dev/null || fail 'Docker daemon is unavailable'
    if ! docker network inspect "$NETWORK_NAME" >"$network_json" 2>/dev/null; then
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
  docker ps --quiet \
    --filter 'label=com.docker.compose.project=cloudhost' \
    --filter 'label=com.docker.compose.service=opensandbox-server' >"$ids_file" 2>/dev/null \
    || fail 'could not inspect the OpenSandbox Compose service'
  [[ "$(wc -l <"$ids_file" | tr -d ' ')" == 1 ]] || fail 'exactly one running cloudhost OpenSandbox server is required'
  local container_id
  IFS= read -r container_id <"$ids_file"
  [[ "$container_id" =~ ^[[:xdigit:]]{12,64}$ ]] || fail 'OpenSandbox container identifier was invalid'
  docker inspect --format '{{json .Mounts}}' "$container_id" >"$mounts_file" 2>/dev/null || fail 'could not inspect OpenSandbox mounts'
  docker inspect --format '{{.HostConfig.NetworkMode}}' "$container_id" >"$network_mode_file" 2>/dev/null || fail 'could not inspect OpenSandbox network mode'
  docker inspect --format '{{.State.StartedAt}}' "$container_id" >"$started_at_file" 2>/dev/null || fail 'could not inspect OpenSandbox start time'
  docker inspect --format '{{.Path}}' "$container_id" >"$executable_file" 2>/dev/null || fail 'could not inspect the OpenSandbox executable'
  args_match="$(docker inspect --format '{{if eq (len .Args) 2}}{{if and (eq (index .Args 0) "--config") (eq (index .Args 1) "/etc/opensandbox/opensandbox.toml")}}true{{end}}{{end}}' "$container_id" 2>/dev/null)" \
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
    "$NFT_BIN" -s -nn list table "$family" cloudhost
  else
    sudo -n "$NFT_BIN" -s -nn list table "$family" cloudhost
  fi
}

current_fingerprint() {
  local inet_table bridge_table
  inet_table="$(nft_table inet)" || fail 'cannot read the dedicated inet nftables table; grant only the documented read-only sudo commands'
  bridge_table="$(nft_table bridge)" || fail 'cannot read the dedicated bridge nftables table; grant only the documented read-only sudo commands'
  printf 'inet\n%s\nbridge\n%s\n' "$inet_table" "$bridge_table" | sha256sum | awk '{print $1}'
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
  check_protected_directory_chain "$STATE_DIR"
  [[ -f "$FINGERPRINT_PATH" && ! -L "$FINGERPRINT_PATH" ]] || fail 'root-protected nftables fingerprint is missing'
  [[ "$(stat -c '%u' "$FINGERPRINT_PATH" 2>/dev/null)" == 0 ]] || fail 'nftables fingerprint must be root-owned'
  local file_mode expected actual
  file_mode="$(stat -c '%a' "$FINGERPRINT_PATH" 2>/dev/null)" || fail 'cannot inspect nftables fingerprint permissions'
  (( (8#$file_mode & 8#022) == 0 )) || fail 'nftables fingerprint is writable by group or others'
  expected="$(cat -- "$FINGERPRINT_PATH")" || fail 'cannot read the root-protected nftables fingerprint'
  [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || fail 'nftables fingerprint format is invalid'
  actual="$(current_fingerprint)"
  [[ "$actual" == "$expected" ]] || fail 'dedicated nftables rules differ from the root-applied fingerprint'
  pass 'dedicated inet and bridge nftables tables match the root-applied fingerprint'
}

lock_policy_state() {
  ((EUID == 0)) || fail 'preload and apply must run as root'
  command -v flock >/dev/null 2>&1 || fail 'flock is required for preload and apply'
  check_protected_directory_chain "$(dirname "$STATE_DIR")"
  if [[ -e "$STATE_DIR" || -L "$STATE_DIR" ]]; then
    [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || fail 'network-policy state path must be a real directory'
  else
    install -d -o root -g root -m 0755 "$STATE_DIR"
  fi
  chown root:root "$STATE_DIR"
  chmod 0755 "$STATE_DIR"
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

install_policy_rules() {
  cat >"$TMP_DIR/rules.nft" <<'NFT'
destroy table inet cloudhost
destroy table bridge cloudhost
add table inet cloudhost
add set inet cloudhost blocked4 { type ipv4_addr; flags interval; elements = { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.0.0.0/24, 192.0.2.0/24, 192.88.99.0/24, 192.168.0.0/16, 198.18.0.0/15, 198.51.100.0/24, 203.0.113.0/24, 224.0.0.0/4, 240.0.0.0/4 } }
add chain inet cloudhost input { type filter hook input priority filter - 1; policy accept; }
add chain inet cloudhost forward { type filter hook forward priority filter - 1; policy accept; }
add rule inet cloudhost input iifname "chbr0" ct state { established, related } accept
add rule inet cloudhost input iifname "chbr0" counter drop
add rule inet cloudhost forward iifname "chbr0" ct direction original ip daddr @blocked4 counter drop
add rule inet cloudhost forward iifname "chbr0" meta nfproto ipv6 counter drop
add table bridge cloudhost
add chain bridge cloudhost forward { type filter hook forward priority filter; policy accept; }
add rule bridge cloudhost forward ibrname "chbr0" obrname "chbr0" counter drop
NFT

  "$NFT_BIN" --check --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables rejected the isolated network policy; no rules were changed'
  "$NFT_BIN" --file "$TMP_DIR/rules.nft" >/dev/null || fail 'nftables could not atomically replace the dedicated network-policy tables'
  local fingerprint temp_fingerprint
  fingerprint="$(current_fingerprint)" || fail 'could not read the applied nftables tables'
  [[ "$fingerprint" =~ ^[[:xdigit:]]{64}$ ]] || fail 'applied nftables fingerprint was invalid'
  temp_fingerprint="$(mktemp "$STATE_DIR/.fingerprint.XXXXXX")"
  printf '%s\n' "$fingerprint" >"$temp_fingerprint"
  chown root:root "$temp_fingerprint"
  chmod 0644 "$temp_fingerprint"
  mv -f -- "$temp_fingerprint" "$FINGERPRINT_PATH"
}

preload_network() {
  lock_policy_state
  # Docker must not be contacted here: its dependency waits for this service.
  config_preflight preload
  install_policy_rules
  pass 'nftables isolation policy preloaded before Docker starts'
}

apply_network() {
  lock_policy_state
  command -v docker >/dev/null 2>&1 || fail 'Docker CLI missing'
  if ! docker network inspect "$NETWORK_NAME" >/dev/null 2>&1; then
    docker network create --driver bridge --ipv6=false --subnet "$SUBNET" \
      --opt "com.docker.network.bridge.name=$BRIDGE_NAME" \
      --opt com.docker.network.bridge.enable_icc=false \
      --opt com.docker.network.bridge.enable_ip_masquerade=true \
      --opt com.docker.network.bridge.host_binding_ipv4=127.0.0.1 \
      "$NETWORK_NAME" >/dev/null || fail 'could not create the dedicated cloudhost Docker network'
  fi

  config_preflight apply
  install_policy_rules
  pass 'dedicated cloudhost network and atomic nftables isolation policy applied'
}

main() {
  local action="${1:-}"
  [[ "$#" == 1 ]] || fail 'usage: network-policy.sh check|preload|apply'
  case "$action" in
    check)
      require_linux
      TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cloudhost-network-check.XXXXXX")"
      chmod 0700 "$TMP_DIR"
      trap cleanup EXIT
      config_preflight check
      server_container_preflight
      check_root_state
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
    *) fail 'usage: network-policy.sh check|preload|apply' ;;
  esac
}

main "$@"
