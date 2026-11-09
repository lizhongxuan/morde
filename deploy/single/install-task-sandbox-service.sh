#!/usr/bin/env bash
set -Eeuo pipefail

die() { printf 'Task Sandbox service install: %s\n' "$*" >&2; exit 1; }

[[ $(uname -s) == Linux ]] || die 'run on Linux'
[[ $EUID == 0 ]] || die 'run as root on Linux'
command -v systemctl >/dev/null 2>&1 || die 'systemd is required'
docker_bin=$(command -v docker) || die 'Docker CLI is required'
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ $repo_root =~ ^/[A-Za-z0-9_./-]+$ ]] || die 'repository path contains unsupported unit-file characters'
[[ $docker_bin =~ ^/[A-Za-z0-9_./-]+$ ]] || die 'Docker CLI path contains unsupported unit-file characters'

for path in /etc /etc/cloudhost /opt; do
  [[ -d $path && ! -L $path && $(stat -c '%u' "$path") == 0 ]] || die "$path must be an existing root-owned real directory"
  mode=$(stat -c '%a' "$path") || die "could not inspect $path permissions"
  (( (8#$mode & 8#022) == 0 )) || die "$path must not be writable by group or others"
done
[[ -f /etc/cloudhost/cloudhost.env && ! -L /etc/cloudhost/cloudhost.env && $(stat -c '%u' /etc/cloudhost/cloudhost.env) == 0 ]] || die '/etc/cloudhost/cloudhost.env must be a root-owned regular file'
[[ $(stat -c '%a' /etc/cloudhost/cloudhost.env) == 600 ]] || die '/etc/cloudhost/cloudhost.env must have mode 0600'
[[ -d /etc/systemd/system && ! -L /etc/systemd/system && $(stat -c '%u' /etc/systemd/system) == 0 ]] || die 'systemd unit directory must be root-owned and real'

if [[ ! -e /etc/opensandbox && ! -L /etc/opensandbox ]]; then
  install -d -o root -g root -m 0755 /etc/opensandbox
fi
[[ -d /etc/opensandbox && ! -L /etc/opensandbox && $(stat -c '%u' /etc/opensandbox) == 0 ]] || die '/etc/opensandbox must be a root-owned real directory'
if [[ ! -e /opt/cloudhost && ! -L /opt/cloudhost ]]; then
  install -d -o root -g root -m 0755 /opt/cloudhost
fi
[[ -d /opt/cloudhost && ! -L /opt/cloudhost && $(stat -c '%u' /opt/cloudhost) == 0 ]] || die '/opt/cloudhost must be a root-owned real directory'

preflight_immutable_file() {
  local source=$1 target=$2
  if [[ -e $target || -L $target ]]; then
    [[ -f $target && ! -L $target && $(stat -c '%u' "$target") == 0 ]] || die "$target must be a root-owned regular file"
    cmp -s "$source" "$target" || die "$target already differs; preserve and review it before replacing"
  fi
}

unit=/etc/systemd/system/morde-task-sandbox.service
[[ ! -L $unit ]] || die "$unit must not be a symlink"
if [[ -e $unit ]] && { [[ ! -f $unit ]] || ! head -n 1 "$unit" | grep -Fxq '# Managed by deploy/single/install-task-sandbox-service.sh'; }; then
  die 'an unmanaged morde-task-sandbox.service already exists'
fi
preflight_immutable_file "$repo_root/deploy/single/opensandbox-task.toml" /etc/opensandbox/opensandbox-task.toml
preflight_immutable_file "$repo_root/deploy/single/task-sandbox-resolv.conf" /etc/cloudhost/task-sandbox-resolv.conf
preflight_immutable_file "$repo_root/deploy/single/task-sandbox-network-policy.sh" /opt/cloudhost/task-sandbox-network-policy.sh

state_dir=/etc/cloudhost/task-sandbox-network-policy
if [[ ! -e $state_dir && ! -L $state_dir ]]; then
  install -d -o root -g root -m 0755 "$state_dir"
fi
[[ -d $state_dir && ! -L $state_dir && $(stat -c '%u' "$state_dir") == 0 ]] || die "$state_dir must be a root-owned real directory"
state_mode=$(stat -c '%a' "$state_dir") || die "could not inspect $state_dir permissions"
(( (8#$state_mode & 8#022) == 0 )) || die "$state_dir must not be writable by group or others"

install_immutable_file() {
  local source=$1 target=$2 mode=$3
  if [[ -e $target || -L $target ]]; then
    [[ -f $target && ! -L $target && $(stat -c '%u' "$target") == 0 ]] || die "$target must be a root-owned regular file"
    if cmp -s "$source" "$target"; then
      return
    fi
    die "$target already differs; preserve and review it before replacing"
  fi
  install -o root -g root -m "$mode" "$source" "$target"
}

install_immutable_file "$repo_root/deploy/single/opensandbox-task.toml" /etc/opensandbox/opensandbox-task.toml 0644
install_immutable_file "$repo_root/deploy/single/task-sandbox-resolv.conf" /etc/cloudhost/task-sandbox-resolv.conf 0644
install_immutable_file "$repo_root/deploy/single/task-sandbox-network-policy.sh" /opt/cloudhost/task-sandbox-network-policy.sh 0755

temp_unit=$(mktemp /etc/systemd/system/.morde-task-sandbox.service.XXXXXX)
trap 'rm -f -- "$temp_unit"' EXIT
sed -e "s|@REPO_ROOT@|$repo_root|g" -e "s|@DOCKER_BIN@|$docker_bin|g" \
  "$repo_root/deploy/single/morde-task-sandbox.service.in" >"$temp_unit" || die 'could not render Task Sandbox systemd unit'
chmod 0644 "$temp_unit"
chown root:root "$temp_unit"
mv -f -- "$temp_unit" "$unit"
trap - EXIT
systemctl daemon-reload

printf 'Task Sandbox service installed but left disabled; no service or network policy was started.\n'
