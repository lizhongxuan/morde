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
  printf '{"supported_features":["task_api","cloud_tasks"]}'
else
  exit 7
fi
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
chmod +x "$TEST_ROOT/bin/uname" "$TEST_ROOT/bin/curl" "$TEST_ROOT/web/portal/node_modules/.bin/vite" \
  "$TEST_ROOT/scripts/setup-builtin-search.sh" "$TEST_ROOT/scripts/start-local-search-forward.sh"

if PATH="$TEST_ROOT/bin:$PATH" TEST_BACKEND=missing "$TEST_ROOT/scripts/start-macos.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing real backend to prevent launch.\n' >&2
  exit 1
fi
grep -q 'No local Morde backend' "$TEST_ROOT/output"
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
