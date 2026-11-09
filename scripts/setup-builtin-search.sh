#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
SEARCH_DIR="$ROOT_DIR/deploy/single/search"
LOCAL_DIR="$ROOT_DIR/.local/builtin-search"
SECRET_FILE="$LOCAL_DIR/searxng.secret"

for command in node npm docker openssl curl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'Missing required command: %s\n' "$command" >&2
    exit 1
  fi
done
NODE_BIN="$(command -v node)"
if ! node -e 'if (+process.versions.node.split(".")[0] < 22) process.exit(1)'; then
  printf 'Built-in search requires Node.js 22 or newer.\n' >&2
  exit 1
fi

SEARCH_COMMAND_TIMEOUT_SECONDS="${CH_SEARCH_COMMAND_TIMEOUT_SECONDS:-120}"
SEARCH_READY_TIMEOUT_SECONDS="${CH_SEARCH_READY_TIMEOUT_SECONDS:-60}"
if [[ ! "$SEARCH_COMMAND_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ || "$SEARCH_COMMAND_TIMEOUT_SECONDS" -gt 600 ]]; then
  printf 'CH_SEARCH_COMMAND_TIMEOUT_SECONDS must be an integer from 1 to 600.\n' >&2
  exit 1
fi
if [[ ! "$SEARCH_READY_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ || "$SEARCH_READY_TIMEOUT_SECONDS" -gt 180 ]]; then
  printf 'CH_SEARCH_READY_TIMEOUT_SECONDS must be an integer from 1 to 180.\n' >&2
  exit 1
fi

# Docker CLI commands can wait indefinitely when Docker Desktop's Engine is
# wedged. Run them in a separate process group so a timeout also stops helper
# processes, while leaving existing containers and volumes untouched.
run_bounded() {
  local timeout_seconds="$1"
  shift
  "$NODE_BIN" -e '
    const { spawn } = require("node:child_process");
    const seconds = Number(process.argv[1]);
    const command = process.argv[2];
    const args = process.argv.slice(3);
    const child = spawn(command, args, { detached: true, stdio: "inherit" });
    let timedOut = false;
    let finished = false;
    let killTimer;
    const finish = (code) => {
      if (finished) return;
      finished = true;
      clearTimeout(timeoutTimer);
      clearTimeout(killTimer);
      process.exitCode = code;
    };
    const signalGroup = (signal) => {
      try { process.kill(-child.pid, signal); } catch (error) { if (error.code !== "ESRCH") throw error; }
    };
    const timeoutTimer = setTimeout(() => {
      timedOut = true;
      signalGroup("SIGTERM");
      killTimer = setTimeout(() => {
        signalGroup("SIGKILL");
        finish(124);
      }, 1000);
    }, seconds * 1000);
    child.on("error", (error) => {
      process.stderr.write(`${error.message}\n`);
      finish(127);
    });
    child.on("exit", (code, signal) => {
      if (timedOut) return;
      finish(code ?? (signal === "SIGINT" ? 130 : 1));
    });
  ' "$timeout_seconds" "$@"
}

if run_bounded "$SEARCH_COMMAND_TIMEOUT_SECONDS" docker compose version >/dev/null 2>&1; then
  :
else
  status=$?
  if [[ "$status" -eq 124 ]]; then
    printf 'Docker Compose version check timed out after %s seconds.\n' "$SEARCH_COMMAND_TIMEOUT_SECONDS" >&2
  else
    printf 'Docker Compose is required.\n' >&2
  fi
  exit 1
fi

if [[ -L "$LOCAL_DIR" ]]; then
  printf 'Search state directory must not be a symlink.\n' >&2
  exit 1
fi
mkdir -p "$LOCAL_DIR"
chmod 700 "$LOCAL_DIR"
if [[ -L "$SECRET_FILE" || ( -e "$SECRET_FILE" && ! -f "$SECRET_FILE" ) ]]; then
  printf 'Search secret must be a regular local file.\n' >&2
  exit 1
fi
if [[ ! -e "$SECRET_FILE" ]]; then
  umask 077
  if ! (set -o noclobber; openssl rand -hex 32 > "$SECRET_FILE"); then
    printf 'Could not create the search secret.\n' >&2
    exit 1
  fi
fi
if [[ ! -s "$SECRET_FILE" ]]; then
  printf 'Search secret must be a non-empty regular local file.\n' >&2
  exit 1
fi
chmod 600 "$SECRET_FILE"

if ! node -e 'const p = require(process.argv[1]); if (p.name !== "mcp-searxng" || p.version !== "2.10.0") process.exit(1)' \
  "$SEARCH_DIR/node_modules/mcp-searxng/package.json" >/dev/null 2>&1; then
  if ! mkdir "$LOCAL_DIR/install.lock" 2>/dev/null; then
    printf 'Another search dependency installation is in progress.\n' >&2
    exit 1
  fi
  trap 'rmdir "$LOCAL_DIR/install.lock" 2>/dev/null || true' EXIT
  if run_bounded "$SEARCH_COMMAND_TIMEOUT_SECONDS" npm --prefix "$SEARCH_DIR" ci --omit=dev --ignore-scripts --no-audit --no-fund; then
    :
  else
    status=$?
    if [[ "$status" -eq 124 ]]; then
      printf 'The pinned search dependency install timed out after %s seconds.\n' "$SEARCH_COMMAND_TIMEOUT_SECONDS" >&2
    else
      printf 'The pinned search dependency install failed.\n' >&2
    fi
    exit 1
  fi
  rmdir "$LOCAL_DIR/install.lock"
  trap - EXIT
fi
node "$SEARCH_DIR/apply-mcp-searxng-patch.mjs"
export CH_SEARXNG_SECRET="$(<"$SECRET_FILE")"
if run_bounded "$SEARCH_COMMAND_TIMEOUT_SECONDS" docker compose -f "$SEARCH_DIR/compose.yaml" up -d; then
  :
else
  status=$?
  if [[ "$status" -eq 124 ]]; then
    printf 'Docker Engine did not complete the search service start request within %s seconds; no containers or volumes were removed.\n' "$SEARCH_COMMAND_TIMEOUT_SECONDS" >&2
  else
    printf 'Docker Compose could not start the search service; no containers or volumes were removed.\n' >&2
  fi
  exit 1
fi

expected_version="2026.11.5+3e95f6cd2"
ready_deadline=$((SECONDS + SEARCH_READY_TIMEOUT_SECONDS))
version_seen=false
while (( SECONDS < ready_deadline )); do
  if response="$(curl -fsS --connect-timeout 1 --max-time 2 http://127.0.0.1:8189/config 2>/dev/null)" && \
    printf '%s' "$response" | node -e 'let s="";process.stdin.on("data",c=>s+=c);process.stdin.on("end",()=>{try{const c=JSON.parse(s);process.exitCode=c.version===process.argv[1]?0:1}catch{process.exitCode=1}})' "$expected_version"; then
    version_seen=true
    break
  fi
  sleep 1
done
if [[ "$version_seen" != true ]]; then
  printf 'SearXNG did not report the pinned version %s on /config within %s seconds.\n' "$expected_version" "$SEARCH_READY_TIMEOUT_SECONDS" >&2
  exit 1
fi

search_seen=false
while (( SECONDS < ready_deadline )); do
  if response="$(curl -fsSG --connect-timeout 1 --max-time 2 --data-urlencode 'q=morde startup health check' \
    --data-urlencode 'format=json' http://127.0.0.1:8189/search 2>/dev/null)" && \
    printf '%s' "$response" | node -e 'let s="";process.stdin.on("data",c=>s+=c);process.stdin.on("end",()=>{try{const c=JSON.parse(s);process.exitCode=Array.isArray(c.results)?0:1}catch{process.exitCode=1}})'; then
    search_seen=true
    break
  fi
  sleep 1
done
if [[ "$search_seen" != true ]]; then
  printf 'SearXNG version is correct, but the JSON /search endpoint did not become ready within %s seconds.\n' "$SEARCH_READY_TIMEOUT_SECONDS" >&2
  exit 1
fi

printf 'Built-in search ready at http://127.0.0.1:8189 (SearXNG %s; JSON search verified).\n' "$expected_version"
printf 'Configure cloud_dsh.builtin_search.script_path as %s/node_modules/mcp-searxng/dist/cli.js.\n' "$SEARCH_DIR"
exit 0
