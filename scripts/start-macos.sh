#!/usr/bin/env bash

set -Eeuo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
  printf 'This launcher is for macOS.\n' >&2
  exit 1
fi

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
PORTAL_DIR="$ROOT_DIR/web/portal"
VITE_BIN="$PORTAL_DIR/node_modules/.bin/vite"
MODE="real"

if [[ $# -gt 1 ]]; then
  printf 'Usage: %s [--demo]\n' "$0" >&2
  exit 1
fi
case "${1:-}" in
  '') ;;
  --demo) MODE="demo" ;;
  -h|--help) printf 'Usage: %s [--demo]\nDefault: connect to an existing local AI backend on port 7080. --demo: mock data only.\n' "$0"; exit 0 ;;
  *) printf 'Usage: %s [--demo]\n' "$0" >&2; exit 1 ;;
esac

NODE_BIN="$(command -v node 2>/dev/null || true)"
NODE_VERSION="unavailable"
if [[ -n "$NODE_BIN" ]]; then
  NODE_VERSION="$("$NODE_BIN" --version 2>/dev/null || true)"
fi
if [[ ! "$NODE_VERSION" =~ ^v?24\.[0-9]+\.[0-9]+$ ]] && command -v brew >/dev/null 2>&1; then
  NODE24_PREFIX="$(brew --prefix node@24 2>/dev/null || true)"
  if [[ -n "$NODE24_PREFIX" && -x "$NODE24_PREFIX/bin/node" ]]; then
    NODE24_VERSION="$("$NODE24_PREFIX/bin/node" --version 2>/dev/null || true)"
    if [[ "$NODE24_VERSION" =~ ^v?24\.[0-9]+\.[0-9]+$ ]]; then
      NODE_BIN="$NODE24_PREFIX/bin/node"
      NODE_VERSION="$NODE24_VERSION"
      PATH="$(dirname -- "$NODE_BIN"):$PATH"
      export PATH
    fi
  fi
fi
if [[ ! "$NODE_VERSION" =~ ^v?24\.[0-9]+\.[0-9]+$ ]]; then
  printf 'This project requires Node.js 24.x (web/portal/package.json; found %s).\n' "${NODE_VERSION:-unavailable}" >&2
  exit 1
fi
if [[ ! -x "$VITE_BIN" ]]; then
  printf 'Portal dependencies are missing. Run npm --prefix web/portal ci, then retry.\n' >&2
  exit 1
fi
if [[ "$MODE" == demo && ! -f "$PORTAL_DIR/public/mockServiceWorker.js" ]]; then
  printf 'The demo worker is missing. Run npm --prefix web/portal run predev, then retry.\n' >&2
  exit 1
fi

backend_ready() {
  command -v curl >/dev/null 2>&1 && curl -fsS --connect-timeout 2 --max-time 3 "$1/api/capabilities" 2>/dev/null |
    "$NODE_BIN" -e 'let s=""; process.stdin.on("data", c => s += c); process.stdin.on("end", () => { try { const f = JSON.parse(s).supported_features; if (!f.includes("cloud_tasks") || !f.includes("conversation_auto_routing_v1")) process.exitCode = 1 } catch { process.exitCode = 1 } })'
}

if [[ "$MODE" == real ]]; then
  LOCAL_BOOTSTRAP="$ROOT_DIR/.local/morde-local/start.sh"
  if ! backend_ready http://127.0.0.1:7080 && [[ -x "$LOCAL_BOOTSTRAP" ]]; then
    "$LOCAL_BOOTSTRAP"
  fi
  if ! backend_ready http://127.0.0.1:7080; then
    printf 'No local Morde backend with automatic conversation routing is available on 127.0.0.1:7080.\n' >&2
    printf 'Upgrade the local Portal, PostgreSQL migrations, isolated DSH and Guardian first. Use --demo only for mock data.\n' >&2
    exit 1
  fi
  if [[ ! -x "$ROOT_DIR/scripts/setup-builtin-search.sh" ]]; then
    printf 'The built-in search setup script is missing.\n' >&2
    exit 1
  fi
  "$ROOT_DIR/scripts/setup-builtin-search.sh"
  if [[ -x "$LOCAL_BOOTSTRAP" ]]; then
    if [[ ! -x "$ROOT_DIR/scripts/start-local-search-forward.sh" ]]; then
      printf 'The local search forwarding script is missing.\n' >&2
      exit 1
    fi
    "$ROOT_DIR/scripts/start-local-search-forward.sh"
  fi
  if backend_ready http://127.0.0.1:5173; then
    printf 'Morde is already available: http://127.0.0.1:5173/\n'
    exit 0
  fi
fi

cd -- "$PORTAL_DIR"
printf 'Morde:       http://127.0.0.1:5173/\n'
printf 'Admin:       http://127.0.0.1:5173/admin/\n'
if [[ "$MODE" == demo ]]; then
  printf 'API mode:    demo only; no real AI or database. Do not enter real API keys.\n'
  MOCK_API=1
else
  printf 'API mode:    existing local Morde backend on 127.0.0.1:7080.\n'
  MOCK_API=0
fi
printf 'Press Ctrl-C to stop.\n'
exec env VITE_MOCK_API="$MOCK_API" "$VITE_BIN" --host 127.0.0.1 --port 5173 --strictPort
