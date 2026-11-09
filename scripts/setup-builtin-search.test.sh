#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REAL_NODE="$(command -v node)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/morde-search-setup-test.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local output="$1"
  local expected="$2"
  [[ "$output" == *"$expected"* ]] || fail "expected output to include: $expected"
}

new_fixture() {
  local root="$TEST_ROOT/$1"
  mkdir -p "$root/scripts" "$root/deploy/single/search/node_modules/mcp-searxng" "$root/bin"
  cp "$SCRIPT_DIR/setup-builtin-search.sh" "$root/scripts/setup-builtin-search.sh"
  printf '{"name":"mcp-searxng","version":"2.10.0"}\n' > "$root/deploy/single/search/node_modules/mcp-searxng/package.json"

  cat > "$root/bin/node" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == "--version" ]]; then
  printf 'v24.1.0\n'
elif [[ "${1:-}" == "-e" ]]; then
  exec "$REAL_NODE" "$@"
else
  # The patcher is covered by its own locked-source tests. This harness
  # verifies setup control flow without changing an npm package.
  exit 0
fi
EOF

  cat > "$root/bin/npm" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${MOCK_NPM_MODE:-}" == "hang" ]]; then
  sleep 60
fi
exit 0
EOF

  cat > "$root/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "${MOCK_DOCKER_LOG:?}"
case "${MOCK_DOCKER_MODE:-}:${2:-}" in
  version_hang:version|up_hang:-f) sleep 60 ;;
esac
case "$*" in
  "compose version") exit 0 ;;
  "compose -f "*" up -d")
    [[ "${MOCK_DOCKER_MODE:-}" != "up_error" ]] || exit 1
    exit 0
    ;;
  *) exit 1 ;;
esac
EOF

  cat > "$root/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
url="${!#}"
case "$url" in
  */config)
    if [[ "${MOCK_CURL_MODE:-}" == "version_mismatch" ]]; then
      printf '{"version":"old"}\n'
    else
      printf '{"version":"2026.11.5+3e95f6cd2"}\n'
    fi
    ;;
  */search)
    case "${MOCK_CURL_MODE:-}" in
      search_unavailable) exit 28 ;;
      invalid_search_json) printf '<html>not json</html>\n' ;;
      *) printf '{"results":[]}\n' ;;
    esac
    ;;
  *) exit 1 ;;
esac
EOF

  chmod +x "$root/bin/node" "$root/bin/npm" "$root/bin/docker" "$root/bin/curl"
  printf '%s\n' "$root"
}

run_setup() {
  local root="$1"
  shift
  env REAL_NODE="$REAL_NODE" PATH="$root/bin:$PATH" \
    MOCK_DOCKER_LOG="$root/docker.log" \
    CH_SEARCH_COMMAND_TIMEOUT_SECONDS="${CH_SEARCH_COMMAND_TIMEOUT_SECONDS:-2}" \
    CH_SEARCH_READY_TIMEOUT_SECONDS="${CH_SEARCH_READY_TIMEOUT_SECONDS:-3}" \
    "$@" "$root/scripts/setup-builtin-search.sh"
}

root="$(new_fixture healthy)"
if output="$(run_setup "$root" 2>&1)"; then
  assert_contains "$output" 'JSON search verified'
  assert_contains "$output" '2026.11.5+3e95f6cd2'
else
  status=$?
  fail "healthy service fixture failed (exit $status): $output"
fi
printf 'PASS: pinned version and JSON search endpoint become ready\n'

root="$(new_fixture version_timeout)"
if output="$(MOCK_DOCKER_MODE=version_hang run_setup "$root" 2>&1)"; then
  fail 'Docker Compose version timeout unexpectedly succeeded'
else
  assert_contains "$output" 'version check timed out'
fi
printf 'PASS: Docker Compose version command timeout is bounded\n'

root="$(new_fixture start_timeout)"
if output="$(MOCK_DOCKER_MODE=up_hang run_setup "$root" 2>&1)"; then
  fail 'Docker Engine start timeout unexpectedly succeeded'
else
  assert_contains "$output" 'Docker Engine did not complete'
  assert_contains "$output" 'no containers or volumes were removed'
fi
if [[ "$(<"$root/docker.log")" == *down* || "$(<"$root/docker.log")" == *' volume rm '* ]]; then
  fail 'timeout cleanup unexpectedly removed containers or volumes'
fi
printf 'PASS: Docker Engine start timeout is bounded without cleanup\n'

root="$(new_fixture engine_error)"
if output="$(MOCK_DOCKER_MODE=up_error run_setup "$root" 2>&1)"; then
  fail 'Docker Compose start error unexpectedly succeeded'
else
  assert_contains "$output" 'could not start the search service'
fi
printf 'PASS: Docker Compose start failures are reported distinctly\n'

root="$(new_fixture npm_timeout)"
printf '{"name":"mcp-searxng","version":"2.9.0"}\n' > "$root/deploy/single/search/node_modules/mcp-searxng/package.json"
if output="$(MOCK_NPM_MODE=hang run_setup "$root" 2>&1)"; then
  fail 'pinned dependency install timeout unexpectedly succeeded'
else
  assert_contains "$output" 'dependency install timed out'
fi
[[ ! -e "$root/.local/builtin-search/install.lock" ]] || fail 'timed-out npm install lock was not released'
printf 'PASS: npm install timeout is bounded and releases its lock\n'

root="$(new_fixture version_mismatch)"
if output="$(MOCK_CURL_MODE=version_mismatch run_setup "$root" 2>&1)"; then
  fail 'unpinned SearXNG version unexpectedly succeeded'
else
  assert_contains "$output" 'did not report the pinned version'
fi
printf 'PASS: unpinned SearXNG version is rejected\n'

root="$(new_fixture search_error)"
if output="$(MOCK_CURL_MODE=search_unavailable run_setup "$root" 2>&1)"; then
  fail 'unavailable JSON search endpoint unexpectedly succeeded'
else
  assert_contains "$output" 'JSON /search endpoint did not become ready'
fi
printf 'PASS: unavailable JSON search endpoint is reported distinctly\n'

root="$(new_fixture search_bad_json)"
if output="$(MOCK_CURL_MODE=invalid_search_json run_setup "$root" 2>&1)"; then
  fail 'non-JSON search response unexpectedly succeeded'
else
  assert_contains "$output" 'JSON /search endpoint did not become ready'
fi
printf 'PASS: invalid JSON search response is rejected\n'
