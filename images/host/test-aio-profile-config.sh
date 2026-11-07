#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/morde-aio-profile-config.py"
test_root="$(mktemp -d /tmp/morde-aio.XXXXXX)"
home="$test_root/home"
mkdir -p "$home"
chmod 0755 "$home"
trap 'rm -rf "$test_root"' EXIT
test_uid="$(id -u)"
test_gid="$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_gid)' "$home")"

run_config() {
  python3 "$script" "$1" --home "$home"
}

run_config_output() {
  python3 "$script" "$1" --home "$home"
}

assert_file() {
  local path="$1" expected="$2"
  test "$(cat "$home/$path")" = "$expected"
}

assert_owner_mode() {
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import os
import stat
import sys

info = os.stat(sys.argv[1], follow_symlinks=False)
assert (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)) == (int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4], 8))
PY
}

# Pre-existing non-empty files and trees survive AIO's first-run copy.
mkdir -p "$home/.config/code-server/vscode" "$home/.jupyter" "$home/.config/matplotlib" \
  "$home/.config/browser/Default" "$home/.config/opencode" "$home/.claude" "$home/.codex" \
  "$home/.agents/skills/custom" "$home/.config/fcitx5/conf"
printf 'user-bashrc-secret' > "$home/.bashrc"
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
assert_owner_mode "$home/.morde-aio-profile-state" "$test_uid" "$test_gid" 700
assert_owner_mode "$home/.morde-aio-profile-state/bashrc.pending" "$test_uid" "$test_gid" 700

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
assert_owner_mode "$home/.bashrc" "$test_uid" "$test_gid" 644
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
assert_owner_mode "$home/.codex/other-settings.json" "$test_uid" "$test_gid" 600
assert_owner_mode "$home/.codex/auth.json" "$test_uid" "$test_gid" 600
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

# New Workspace volumes become the default for empty desktop directories and Chromium downloads.
workspace_home="$test_root/workspace-home"
mkdir -p "$workspace_home/workspace" "$workspace_home/.config/browser/Default"
printf '{}' > "$workspace_home/.config/browser/Default/Preferences"
MORDE_WORKSPACE_LAYOUT=workspace python3 "$script" workspace-defaults --home "$workspace_home"
grep -Fq 'XDG_DOWNLOAD_DIR="$HOME/workspace/Downloads"' "$workspace_home/.config/user-dirs.dirs"
grep -Fq 'XDG_DESKTOP_DIR="$HOME/workspace/Desktop"' "$workspace_home/.config/user-dirs.dirs"
test -d "$workspace_home/workspace/Downloads"
python3 - "$workspace_home/.config/browser/Default/Preferences" "$workspace_home/workspace/Downloads" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    preferences = json.load(source)
assert preferences["download"]["default_directory"] == sys.argv[2]
assert preferences["download"]["directory_upgrade"] is True
PY

# Existing profile files stay at their old path until W22 handles their data.
preserve_home="$test_root/preserve-home"
mkdir -p "$preserve_home/workspace" "$preserve_home/Downloads" "$preserve_home/.config/browser/Default"
printf 'keep-existing-download' > "$preserve_home/Downloads/keep.txt"
printf 'XDG_DOWNLOAD_DIR="$HOME/Downloads"\n' > "$preserve_home/.config/user-dirs.dirs"
printf '{"download":{"default_directory":"%s/Downloads"}}\n' "$preserve_home" > "$preserve_home/.config/browser/Default/Preferences"
MORDE_WORKSPACE_LAYOUT=workspace python3 "$script" workspace-defaults --home "$preserve_home"
grep -Fq 'XDG_DOWNLOAD_DIR="$HOME/Downloads"' "$preserve_home/.config/user-dirs.dirs"
python3 - "$preserve_home/.config/browser/Default/Preferences" "$preserve_home" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    preferences = json.load(source)
assert preferences["download"]["default_directory"] == sys.argv[2] + "/Downloads"
PY
test "$(cat "$preserve_home/Downloads/keep.txt")" = keep-existing-download

# Legacy Host volumes retain the profile's existing defaults.
legacy_home="$test_root/legacy-home"
mkdir -p "$legacy_home/workspace" "$legacy_home/Downloads"
printf 'old-downloads' > "$legacy_home/Downloads/keep.txt"
MORDE_WORKSPACE_LAYOUT=legacy python3 "$script" workspace-defaults --home "$legacy_home"
test ! -e "$legacy_home/.config/user-dirs.dirs"
test "$(cat "$legacy_home/Downloads/keep.txt")" = old-downloads

# Only Chromium's generated DeferredBrowserMetrics files are removed; user
# files, unrelated .pma files, and symlinks remain untouched.
metrics_home="$test_root/metrics-home"
metrics_dir="$metrics_home/.config/browser/DeferredBrowserMetrics"
mkdir -p "$metrics_dir"
printf 'generated-metrics' > "$metrics_dir/BrowserMetrics-6AED6451-21D0.pma"
printf 'generated-metrics-short-id' > "$metrics_dir/BrowserMetrics-6AED5594-541.pma"
printf 'keep-user-file' > "$metrics_dir/keep.pma"
printf 'keep-nonmatching' > "$metrics_dir/BrowserMetrics-not-a-metric.pma"
printf 'outside-target' > "$test_root/outside-metric-target"
ln -s "$test_root/outside-metric-target" "$metrics_dir/BrowserMetrics-6AED6452-21D0.pma"
removed="$(python3 "$script" prune-browser-metrics --home "$metrics_home")"
test "$removed" = 2
test ! -e "$metrics_dir/BrowserMetrics-6AED6451-21D0.pma"
test ! -e "$metrics_dir/BrowserMetrics-6AED5594-541.pma"
test -f "$metrics_dir/keep.pma"
test -f "$metrics_dir/BrowserMetrics-not-a-metric.pma"
test -L "$metrics_dir/BrowserMetrics-6AED6452-21D0.pma"
test "$(cat "$test_root/outside-metric-target")" = outside-target

# Chromium's singleton symlinks belong to one container process and become
# stale when a Host is recreated. Startup removes only those links and keeps
# the browser profile contents intact.
browser_lock_home="$test_root/browser-lock-home"
mkdir -p "$browser_lock_home/.config/browser/Default"
printf 'keep-browser-profile' > "$browser_lock_home/.config/browser/Default/Bookmarks"
ln -s 'oldhost-394' "$browser_lock_home/.config/browser/SingletonLock"
ln -s '123456789' "$browser_lock_home/.config/browser/SingletonCookie"
ln -s '/tmp/org.chromium.Chromium.StaleSocket' "$browser_lock_home/.config/browser/SingletonSocket"
python3 "$script" prepare --home "$browser_lock_home"
for name in SingletonLock SingletonCookie SingletonSocket; do
  test ! -L "$browser_lock_home/.config/browser/$name"
done
test "$(cat "$browser_lock_home/.config/browser/Default/Bookmarks")" = keep-browser-profile

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

echo 'PASS: AIO profile overwrite recovery, stale Chromium locks, initial empty config, interrupted startup, deletion, Workspace separation, and fail-closed symlink checks'
