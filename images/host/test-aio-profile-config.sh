#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/morde-aio-profile-config.py"
test_root="$(mktemp -d)"
home="$test_root/home"
mkdir -p "$home"
chmod 0755 "$home"
trap 'rm -rf "$test_root"' EXIT

run_config() {
  python3 "$script" "$1" --home "$home"
}

assert_file() {
  local path="$1" expected="$2"
  test "$(cat "$home/$path")" = "$expected"
}

# Pre-existing non-empty files and trees survive AIO's first-run copy.
mkdir -p "$home/.config/code-server/vscode" "$home/.jupyter" "$home/.config/matplotlib" \
  "$home/.config/browser/Default" "$home/.config/opencode" "$home/.claude" "$home/.codex" \
  "$home/.agents/skills/custom" "$home/.config/fcitx5/conf"
printf 'user-bashrc-secret' > "$home/.bashrc"
chown 1000:1000 "$home/.bashrc"
printf 'user-vscode-setting' > "$home/.config/code-server/vscode/settings.json"
printf 'user-jupyter-setting' > "$home/.jupyter/custom.py"
printf 'user-matplotlib-setting' > "$home/.config/matplotlib/matplotlibrc"
printf 'user-browser-setting' > "$home/.config/browser/Default/Preferences"
printf 'user-opencode-setting' > "$home/.config/opencode/config.json"
printf 'user-claude-setting' > "$home/.claude/settings.json"
printf 'user-codex-setting' > "$home/.codex/config.toml"
printf 'user-catalog-setting' > "$home/.codex/model-catalog.json"
printf 'user-codex-extra-setting' > "$home/.codex/other-settings.json"
printf '%s\n' '{"morde_test_fixture":"SYNTHETIC_NOT_A_CREDENTIAL"}' > "$home/.codex/auth.json"
chmod 0600 "$home/.codex/other-settings.json"
chmod 0600 "$home/.codex/auth.json"
mkdir -p "$home/.codex/tmp/arg0-linux"
printf 'temporary-lock' > "$home/.codex/tmp/arg0-linux/.lock"
ln -s "$script" "$home/.codex/tmp/arg0-linux/apply_patch"
printf 'user-skill' > "$home/.agents/skills/custom/SKILL.md"
printf 'user-fcitx-setting' > "$home/.config/fcitx5/conf/custom.conf"
mkdir -p "$home/workspace"
printf 'workspace-data' > "$home/workspace/keep.txt"
run_config prepare
test "$(stat -c '%u:%a' "$home/.morde-aio-profile-state")" = 0:700
test "$(stat -c '%u:%a' "$home/.morde-aio-profile-state/bashrc.pending")" = 0:700

# Simulate the fixed image's copy/overwrite stage. It runs before pre-services.
printf 'aio-default' > "$home/.bashrc"
printf 'aio-default' > "$home/.config/code-server/vscode/settings.json"
printf 'aio-default' > "$home/.config/code-server/vscode/default.json"
printf 'aio-default' > "$home/.jupyter/custom.py"
printf 'aio-default' > "$home/.jupyter/default.py"
printf 'aio-default' > "$home/.config/matplotlib/matplotlibrc"
printf 'aio-default' > "$home/.config/browser/Default/Preferences"
printf 'aio-default' > "$home/.config/opencode/config.json"
printf 'aio-default' > "$home/.claude/settings.json"
printf 'aio-default' > "$home/.codex/config.toml"
printf 'aio-default' > "$home/.codex/model-catalog.json"
printf 'aio-default' > "$home/.codex/other-settings.json"
printf 'aio-default' > "$home/.codex/auth.json"
printf 'aio-default' > "$home/.agents/skills/custom/SKILL.md"
printf 'aio-default' > "$home/.agents/skills/custom/aio-default.md"
printf 'aio-default' > "$home/.config/fcitx5/conf/custom.conf"
printf 'aio-profile' > "$home/.config/fcitx5/profile"
python3 - "$home/.config/code-server/vscode/code-server-ipc.sock" <<'PY'
import socket
import sys

ipc = socket.socket(socket.AF_UNIX)
ipc.bind(sys.argv[1])
ipc.close()
PY
run_config restore

assert_file .bashrc user-bashrc-secret
test "$(stat -c '%u:%g' "$home/.bashrc")" = 1000:1000
assert_file .config/code-server/vscode/settings.json user-vscode-setting
test ! -e "$home/.config/code-server/vscode/default.json"
test ! -e "$home/.config/code-server/vscode/code-server-ipc.sock"
assert_file .jupyter/custom.py user-jupyter-setting
test ! -e "$home/.jupyter/default.py"
assert_file .config/matplotlib/matplotlibrc user-matplotlib-setting
assert_file .config/browser/Default/Preferences user-browser-setting
assert_file .config/opencode/config.json user-opencode-setting
assert_file .claude/settings.json user-claude-setting
assert_file .codex/config.toml user-codex-setting
assert_file .codex/model-catalog.json user-catalog-setting
assert_file .codex/other-settings.json user-codex-extra-setting
assert_file .codex/auth.json '{"morde_test_fixture":"SYNTHETIC_NOT_A_CREDENTIAL"}'
test "$(stat -c '%a' "$home/.codex/other-settings.json")" = 600
test "$(stat -c '%a' "$home/.codex/auth.json")" = 600
test ! -e "$home/.codex/tmp"
assert_file .agents/skills/custom/SKILL.md user-skill
test ! -e "$home/.agents/skills/custom/aio-default.md"
assert_file .config/fcitx5/conf/custom.conf user-fcitx-setting
test ! -e "$home/.config/fcitx5/profile"
assert_file workspace/keep.txt workspace-data

# Codex recreates disposable symlinks under .codex/tmp; they are excluded from
# the next profile snapshot so they cannot block a later Host restart.
codex_home="$test_root/codex-home"
mkdir -p "$codex_home/.codex"
printf 'user-codex-setting' > "$codex_home/.codex/config.toml"
python3 "$script" prepare --home "$codex_home"
printf 'aio-default' > "$codex_home/.codex/config.toml"
python3 "$script" restore --home "$codex_home"
mkdir -p "$codex_home/.codex/tmp/arg0-linux"
ln -s "$script" "$codex_home/.codex/tmp/arg0-linux/apply_patch"
python3 "$script" prepare --home "$codex_home"
test -d "$codex_home/.morde-aio-profile-state/codex.pending"
test ! -e "$codex_home/.morde-aio-profile-state/codex.pending/content/tmp"

# A user edit after startup becomes a durable pending snapshot. A simulated
# interrupted AIO startup leaves the snapshot for the next container start.
printf 'user-edited-after-first-run' > "$home/.bashrc"
run_config prepare
printf 'partially-written-by-aio' > "$home/.bashrc"
run_config prepare
run_config restore
assert_file .bashrc user-edited-after-first-run

# An initially empty config is not backed up and therefore is initialized by AIO.
empty_home="$test_root/empty-home"
mkdir -p "$empty_home"
: > "$empty_home/.bashrc"
python3 "$script" prepare --home "$empty_home"
test ! -e "$empty_home/.morde-aio-profile-state/bashrc.pending"
printf 'aio-initial-default' > "$empty_home/.bashrc"
python3 "$script" restore --home "$empty_home"
test "$(cat "$empty_home/.bashrc")" = aio-initial-default

# A deliberate deletion after baseline creation is also restored after AIO recreates it.
rm -f "$home/.config/matplotlib/matplotlibrc"
run_config prepare
printf 'aio-recreated-config' > "$home/.config/matplotlib/matplotlibrc"
run_config restore
test ! -e "$home/.config/matplotlib/matplotlibrc"

# Symlink targets are rejected before the AIO process can mutate an external file.
outside="$test_root/outside"
printf 'leave-unchanged' > "$outside"
rm -f "$home/.bashrc"
ln -s "$outside" "$home/.bashrc"
if run_config prepare >/dev/null 2>&1; then
  echo 'expected a symlinked managed config to fail closed' >&2
  exit 1
fi
test "$(cat "$outside")" = leave-unchanged

echo 'PASS: AIO profile overwrite recovery, initial empty config, interrupted startup, deletion, Workspace separation, and fail-closed symlink checks'
