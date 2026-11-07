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
if ! node -e 'if (+process.versions.node.split(".")[0] < 22) process.exit(1)'; then
  printf 'Built-in search requires Node.js 22 or newer.\n' >&2
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  printf 'Docker Compose is required.\n' >&2
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
  npm --prefix "$SEARCH_DIR" ci --omit=dev --ignore-scripts --no-audit --no-fund
  rmdir "$LOCAL_DIR/install.lock"
  trap - EXIT
fi
node "$SEARCH_DIR/apply-mcp-searxng-patch.mjs"
export CH_SEARXNG_SECRET="$(<"$SECRET_FILE")"
docker compose -f "$SEARCH_DIR/compose.yaml" up -d
for ((attempt = 0; attempt < 30; attempt++)); do
  if curl -fsS --connect-timeout 1 --max-time 2 http://127.0.0.1:8189/ >/dev/null 2>&1; then
    printf 'Built-in search ready at http://127.0.0.1:8189.\n'
    printf 'Configure cloud_dsh.builtin_search.script_path as %s/node_modules/mcp-searxng/dist/cli.js.\n' "$SEARCH_DIR"
    exit 0
  fi
  sleep 1
done
printf 'SearXNG did not become ready on 127.0.0.1:8189.\n' >&2
exit 1
