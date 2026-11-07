#!/usr/bin/env bash
set -Eeuo pipefail

TEST_ROOT="$(mktemp -d)"
trap 'rm -rf -- "$TEST_ROOT"' EXIT
mkdir -p "$TEST_ROOT/bin"
touch "$TEST_ROOT/ssh.config"
sed "s|^SSH_CONFIG=.*|SSH_CONFIG=\"$TEST_ROOT/ssh.config\"|" \
  "$(dirname "$0")/start-local-search-forward.sh" > "$TEST_ROOT/forward.sh"
cat > "$TEST_ROOT/bin/limactl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *systemctl*) [[ "${TEST_GUARDIAN:-ready}" == ready ]] ;;
  *portal.yaml*) [[ "${TEST_CONFIG:-ready}" == ready && "${TEST_NODE_VERSION:-24}" -ge 22 ]] ;;
  */config*) [[ "${TEST_SEARCH:-ready}" == ready ]] ;;
  *) exit 2 ;;
esac
EOF
cat > "$TEST_ROOT/bin/ssh" <<'EOF'
#!/usr/bin/env bash
touch "$TEST_SSH_CALLED"
exit 1
EOF
chmod +x "$TEST_ROOT/bin/limactl" "$TEST_ROOT/bin/ssh" "$TEST_ROOT/forward.sh"
export PATH="$TEST_ROOT/bin:$PATH" TEST_SSH_CALLED="$TEST_ROOT/ssh-called"

"$TEST_ROOT/forward.sh"
if [[ -e "$TEST_SSH_CALLED" ]]; then
  printf 'An already healthy local tunnel must not be restarted.\n' >&2
  exit 1
fi
if TEST_GUARDIAN=missing "$TEST_ROOT/forward.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing Guardian to prevent forwarding.\n' >&2
  exit 1
fi
grep -q 'Guardian is not ready' "$TEST_ROOT/output"
if TEST_CONFIG=missing "$TEST_ROOT/forward.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected missing search configuration to prevent forwarding.\n' >&2
  exit 1
fi
grep -q 'needs enabled built-in search' "$TEST_ROOT/output"
if TEST_NODE_VERSION=20 "$TEST_ROOT/forward.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected an unsupported Node.js version to prevent forwarding.\n' >&2
  exit 1
fi
grep -q 'Node.js 22 or newer' "$TEST_ROOT/output"
if TEST_SEARCH=missing "$TEST_ROOT/forward.sh" > "$TEST_ROOT/output" 2>&1; then
  printf 'Expected a failed tunnel to report failure.\n' >&2
  exit 1
fi
grep -q 'Could not forward local search' "$TEST_ROOT/output"
printf 'Local search forwarding checks passed.\n'
