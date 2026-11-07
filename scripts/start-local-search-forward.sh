#!/usr/bin/env bash
set -Eeuo pipefail

VM_NAME=morde-f01-u2604
SSH_CONFIG="$HOME/.lima/$VM_NAME/ssh.config"
for command in limactl ssh; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'Missing required local runtime command: %s\n' "$command" >&2
    exit 1
  fi
done
if [[ ! -r "$SSH_CONFIG" ]]; then
  printf 'The existing Morde virtual machine SSH configuration is unavailable.\n' >&2
  exit 1
fi
if ! limactl shell "$VM_NAME" -- sh -c 'for unit in morde-dsh-containerd.service morde-dsh-docker.service morde-dsh-guardian.service; do systemctl is-active --quiet "$unit" || exit 1; done'; then
  printf 'The isolated DSH runtime or Guardian is not ready.\n' >&2
  exit 1
fi
if ! limactl shell "$VM_NAME" -- sudo -n bash -c 'set -a; source /etc/morde-local/service.env; set +a; exec python3 -c "$1"' -- '
import os, re, subprocess, sys, yaml
with open("/etc/morde-local/portal.yaml", encoding="utf-8") as f:
    cfg = yaml.safe_load(f) or {}
runtime = cfg.get("cloud_dsh") or {}
search = runtime.get("builtin_search") or {}
node = os.path.expandvars(str(search.get("node_path") or ""))
script = os.path.expandvars(str(search.get("script_path") or ""))
ok = runtime.get("enabled") is True and search.get("enabled") is True
ok = ok and os.path.isfile(node) and os.path.isfile(script)
ok = ok and search.get("searxng_url") == "http://127.0.0.1:8189"
if ok:
    try:
        version = subprocess.run([node, "--version"], check=True, capture_output=True, text=True, timeout=3).stdout.strip()
        match = re.fullmatch(r"v?(\d+)\.\d+\.\d+(?:[-+].*)?", version)
        ok = bool(match) and int(match.group(1)) >= 22
    except (OSError, subprocess.SubprocessError, ValueError):
        ok = False
sys.exit(0 if ok else 1)
'; then
  printf 'The local Portal needs enabled built-in search and Node.js 22 or newer; initialize or repair the local runtime first.\n' >&2
  exit 1
fi
search_ready() {
  limactl shell "$VM_NAME" -- sh -c 'curl -fsS --connect-timeout 1 --max-time 2 http://127.0.0.1:8189/config | python3 -c '\''import json,sys; x=json.load(sys.stdin); sys.exit(0 if x.get("version") == "2026.11.5+3e95f6cd2" else 1)'\''' >/dev/null 2>&1
}
if search_ready; then
  exit 0
fi
nohup ssh -F "$SSH_CONFIG" -o ControlPath=none -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -N \
  -R 127.0.0.1:8189:127.0.0.1:8189 "lima-$VM_NAME" </dev/null >/dev/null 2>&1 &
forward_pid=$!
for ((attempt = 0; attempt < 10; attempt++)); do
  if search_ready; then
    exit 0
  fi
  if ! kill -0 "$forward_pid" 2>/dev/null; then
    break
  fi
  sleep 1
done
kill "$forward_pid" 2>/dev/null || true
printf 'Could not forward local search into the Morde VM; check whether port 8189 is already in use.\n' >&2
exit 1
