#!/usr/bin/env bash

set -Eeuo pipefail

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf -- "$TEST_ROOT"' EXIT
mkdir -p "$TEST_ROOT/scripts" "$TEST_ROOT/web/portal/node_modules/.bin" "$TEST_ROOT/web/portal/public" "$TEST_ROOT/bin"
cp "$(dirname "$0")/start-macos.sh" "$TEST_ROOT/scripts/"
touch "$TEST_ROOT/web/portal/public/mockServiceWorker.js"
cat > "$TEST_ROOT/bin/uname" <<'EOF'
#!/usr/bin/env bash
printf 'Darwin\n'
EOF
cat > "$TEST_ROOT/bin/curl" <<'EOF'
#!/usr/bin/env bash
if [[ "${*: -1}" == http://127.0.0.1:5173/* ]]; then
  [[ "${TEST_FRONTEND:-}" == ready ]] || exit 7
elif [[ "${TEST_BACKEND:-}" != ready ]]; then
  exit 7
fi
if [[ "${TEST_BACKEND:-}" == ready ]]; then
  printf '{"supported_features":["task_api","cloud_tasks","conversation_auto_routing_v1"]}'
elif [[ "${TEST_BACKEND:-}" == legacy ]]; then
  printf '{"supported_features":["task_api","cloud_tasks"]}'
else
  exit 7
fi
EOF
cat > "$TEST_ROOT/bin/node" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == --version ]]; then
  printf '%s\n' "${TEST_NODE_VERSION:-v24.15.0}"
elif [[ "${1:-}" == -e ]]; then
  payload="$(cat)"
  [[ "$payload" == *'"cloud_tasks"'* && "$payload" == *'"conversation_auto_routing_v1"'* ]]
else
  exit 2
fi
EOF
cat > "$TEST_ROOT/bin/brew" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == --prefix && "${2:-}" == node@24 && -n "${TEST_BREW_NODE24_PREFIX:-}" ]]; then
  printf '%s\n' "$TEST_BREW_NODE24_PREFIX"
  exit 0
fi
exit 1
EOF
cat > "$TEST_ROOT/scripts/setup-builtin-search.sh" <<'EOF'
#!/usr/bin/env bash
printf 'SEARCH_SETUP\n'
EOF
cat > "$TEST_ROOT/scripts/start-local-search-forward.sh" <<'EOF'
#!/usr/bin/env bash
printf 'SEARCH_FORWARD\n'
EOF
cat > "$TEST_ROOT/web/portal/node_modules/.bin/vite" <<'EOF'
#!/usr/bin/env bash
printf 'VITE_MOCK_API=%s\n' "$VITE_MOCK_API"
EOF
chmod +x "$TEST_ROOT/bin/uname" "$TEST_ROOT/bin/curl" "$TEST_ROOT/bin/node" "$TEST_ROOT/bin/brew" "$TEST_ROOT/web/portal/node_modules/.bin/vite" \
  "$TEST_ROOT/scripts/setup-builtin-search.sh" "$TEST_ROOT/scripts/start-local-search-forward.sh"

mkdir -p "$TEST_ROOT/bootstrap"
cp "$(dirname "$0")/../.local/morde-local/start.sh" "$TEST_ROOT/bootstrap/start.sh"
cat > "$TEST_ROOT/bin/docker" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == info && "${TEST_DOCKER_MODE:-}" == hang ]]; then
  exec sleep 3
fi
exit 1
EOF
chmod +x "$TEST_ROOT/bin/docker" "$TEST_ROOT/bootstrap/start.sh"
if PATH="$TEST_ROOT/bin:$PATH" TEST_DOCKER_MODE=hang MORDE_LOCAL_DOCKER_TIMEOUT_SECONDS=0.1 \
  "$TEST_ROOT/bootstrap/start.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected an unresponsive Docker Engine to stop the local bootstrap.\n' >&2
  exit 1
fi
grep -q 'Docker Desktop Engine is unavailable or did not respond in time' "$TEST_ROOT/output"
if grep -q 'Please start Docker Desktop' "$TEST_ROOT/output"; then
  printf 'The local bootstrap must distinguish an unresponsive Engine from a stopped app.\n' >&2
  exit 1
fi

if PATH="$TEST_ROOT/bin:$PATH" TEST_NODE_VERSION=v26.8.2 "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected an unsupported Node.js version to prevent launch.\n' >&2
  exit 1
fi
grep -q 'requires Node.js 24.x.*found v26.8.2' "$TEST_ROOT/output"
mkdir -p "$TEST_ROOT/node24/bin"
cat > "$TEST_ROOT/node24/bin/node" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == --version ]]; then
  printf 'v24.22.2\n'
elif [[ "${1:-}" == -e ]]; then
  payload="$(cat)"
  [[ "$payload" == *'"cloud_tasks"'* && "$payload" == *'"conversation_auto_routing_v1"'* ]]
else
  exit 2
fi
EOF
chmod +x "$TEST_ROOT/node24/bin/node"
PATH="$TEST_ROOT/bin:$PATH" TEST_NODE_VERSION=v26.8.2 TEST_BREW_NODE24_PREFIX="$TEST_ROOT/node24" TEST_BACKEND=ready \
  "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output"
grep -q '^SEARCH_SETUP$' "$TEST_ROOT/output"
grep -q '^VITE_MOCK_API=0$' "$TEST_ROOT/output"
chmod -x "$TEST_ROOT/web/portal/node_modules/.bin/vite"
if PATH="$TEST_ROOT/bin:$PATH" "$TEST_ROOT/scripts/start-macos.sh" --demo > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing portal dependencies to prevent launch.\n' >&2
  exit 1
fi
grep -q 'Portal dependencies are missing.*npm --prefix web/portal ci' "$TEST_ROOT/output"
chmod +x "$TEST_ROOT/web/portal/node_modules/.bin/vite"
rm "$TEST_ROOT/web/portal/public/mockServiceWorker.js"
if PATH="$TEST_ROOT/bin:$PATH" "$TEST_ROOT/scripts/start-macos.sh" --demo > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected a missing demo worker to prevent launch.\n' >&2
  exit 1
fi
grep -q 'demo worker is missing.*npm --prefix web/portal run predev' "$TEST_ROOT/output"
touch "$TEST_ROOT/web/portal/public/mockServiceWorker.js"
if PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=missing "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing real backend to prevent launch.\n' >&2
  exit 1
fi
grep -q 'No local Morde backend' "$TEST_ROOT/output"
if PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=legacy "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected a legacy backend without automatic conversation routing to prevent launch.\n' >&2
  exit 1
fi
grep -q 'automatic conversation routing' "$TEST_ROOT/output"
PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=ready "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output"
grep -q '^SEARCH_SETUP$' "$TEST_ROOT/output"
grep -q '^VITE_MOCK_API=0$' "$TEST_ROOT/output"
PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=missing "$TEST_ROOT/scripts/start-macos.sh" --demo > "$TEST_ROOT/output"
grep -q '^VITE_MOCK_API=1$' "$TEST_ROOT/output"
if grep -q '^SEARCH_SETUP$' "$TEST_ROOT/output"; then
  printf 'Demo mode must not start search.\n' >&2
  exit 1
fi

mkdir -p "$TEST_ROOT/.local/morde-local"
cat > "$TEST_ROOT/.local/morde-local/start.sh" <<'EOF'
#!/usr/bin/env bash
printf 'LOCAL_BOOTSTRAP\n'
EOF
chmod +x "$TEST_ROOT/.local/morde-local/start.sh"
PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=ready TEST_FRONTEND=ready "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output"
grep -q '^SEARCH_FORWARD$' "$TEST_ROOT/output"
grep -q 'Morde is already available' "$TEST_ROOT/output"
if grep -q '^VITE_MOCK_API=' "$TEST_ROOT/output"; then
  printf 'An existing portal must not be started a second time.\n' >&2
  exit 1
fi
chmod -x "$TEST_ROOT/scripts/start-local-search-forward.sh"
if PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=ready "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing search forwarder to prevent launch.\n' >&2
  exit 1
fi
grep -q 'forwarding script is missing' "$TEST_ROOT/output"
printf 'macOS launcher checks passed.\n'
