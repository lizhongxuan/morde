#!/usr/bin/env bash
set -Eeuo pipefail

die() { printf 'Portal service install: %s\n' "$*" >&2; exit 1; }

workspace_sync_enabled_from_config() {
  awk '
    $0 == "workspace_sync_node:" { in_sync=1; next }
    in_sync && /^[^[:space:]]/ { in_sync=0 }
    in_sync && $1 == "enabled:" { print $2; exit }
  ' "$repo_root/deploy/single/portal.yaml"
}

workspace_sync_current_root_from_config() {
  awk '
    $0 == "workspace_sync_node:" { in_sync=1; next }
    in_sync && /^[^[:space:]]/ { in_sync=0 }
    in_sync && $1 == "workspace_root:" {
      sub(/^[[:space:]]*workspace_root:[[:space:]]*/, "")
      gsub(/^["\047]|["\047]$/, "")
      print
      exit
    }
  ' "$repo_root/deploy/single/portal.yaml"
}

check_workspace_sync_current_root_config() {
  local configured_root
  configured_root=$(workspace_sync_current_root_from_config)
  [[ -n ${CH_WORKSPACE_SYNC_CURRENT_FILES_DIR:-} && -n $configured_root ]] || die 'Workspace current-files root is missing from config or environment'
  if [[ $configured_root == '${CH_WORKSPACE_SYNC_CURRENT_FILES_DIR}' ]]; then
    configured_root=$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR
  fi
  [[ $configured_root == "$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR" ]] || die 'portal.yaml workspace_root must match CH_WORKSPACE_SYNC_CURRENT_FILES_DIR'
}

sync_mount_units=()
workspace_sync_mount_units() {
  local path device mount_unit
  local -A sync_devices=()
  sync_mount_units=()
  command -v mountpoint >/dev/null && command -v systemd-escape >/dev/null || die 'mountpoint and systemd-escape are required for Workspace sync'
  for path in "$@"; do
    [[ $path == /* && -d $path && ! -L $path && $(realpath -e -- "$path") == "$path" ]] || die 'Workspace sync roots must be existing absolute directories without symlinks'
    mountpoint -q -- "$path" || die "Workspace sync root is not a dedicated mount point: $path"
    device=$(stat -c '%d' "$path")
    [[ ! ${sync_devices[$device]+present} ]] || die 'Workspace sync roots must use separate filesystems'
    sync_devices[$device]=$path
    mount_unit=$(systemd-escape --path --suffix=mount "$path") || die "could not resolve the mount unit for $path"
    [[ $mount_unit == *.mount && $mount_unit != */* && $mount_unit != *' '* ]] || die "invalid systemd mount unit for $path"
    systemctl is-active --quiet "$mount_unit" || die "Workspace sync mount unit is not active: $mount_unit"
    sync_mount_units+=("$mount_unit")
  done
}

check_workspace_sync_mount_bindings() {
  local enabled bindings ordered mount_unit
  enabled=$(workspace_sync_enabled_from_config)
  [[ $enabled == true || $enabled == false ]] || die 'workspace_sync_node.enabled must be a literal true or false'
  check_workspace_sync_current_root_config
  bindings=$(systemctl show --property=BindsTo --value morde-portal.service) || die 'could not inspect Portal mount dependencies'
  ordered=$(systemctl show --property=After --value morde-portal.service) || die 'could not inspect Portal mount ordering'
  local -a roots=("${CH_WORKSPACE_SYNC_CURRENT_FILES_DIR:-}")
  if [[ $enabled == true ]]; then
    roots=("${CH_WORKSPACE_STORAGE_ROOT_DIR:-}" "${CH_WORKSPACE_SYNC_CURRENT_FILES_DIR:-}" "${CH_WORKSPACE_SYNC_NODE_STATE_DIR:-}" "${CH_WORKSPACE_SYNC_RELAY_STATE_DIR:-}")
  fi
  workspace_sync_mount_units "${roots[@]}"
  for mount_unit in "${sync_mount_units[@]}"; do
    case " $bindings " in *" $mount_unit "*) ;; *) die "Portal unit is not bound to Workspace sync mount $mount_unit; rerun install-portal-service.sh" ;; esac
    case " $ordered " in *" $mount_unit "*) ;; *) die "Portal unit does not order after Workspace sync mount $mount_unit; rerun install-portal-service.sh" ;; esac
  done
}

validate_nftables_base_unit() {
  local fragment loaded_dropins path
  fragment=$(systemctl show --property=FragmentPath --value nftables.service) || die 'could not inspect nftables.service'
  [[ -f $fragment && ! -L $fragment && $(stat -c '%u' "$fragment") == 0 ]] || die 'nftables.service must use a root-owned regular unit file'
  [[ $(grep -Fxc 'ExecStart=/usr/sbin/nft -f /etc/nftables.conf' "$fragment") == 1 ]] || die 'unsupported nftables.service ExecStart; expected /usr/sbin/nft -f /etc/nftables.conf'
  [[ $(grep -Fxc 'ExecReload=/usr/sbin/nft -f /etc/nftables.conf' "$fragment") == 1 ]] || die 'unsupported nftables.service ExecReload; expected /usr/sbin/nft -f /etc/nftables.conf'
  [[ $(grep -Fxc 'ExecStop=/usr/sbin/nft flush ruleset' "$fragment") == 1 ]] || die 'unsupported nftables.service ExecStop; refusing a lifecycle override that cannot control ruleset flush'
  ! grep -Eq '^[[:space:]]*(ExecStartPre|ExecStartPost|ExecStopPost)=' "$fragment" || die 'nftables.service has an unsupported pre-start or post-start/stop command'
  loaded_dropins=$(systemctl show --property=DropInPaths --value nftables.service) || die 'could not inspect nftables.service drop-ins'
  for path in $loaded_dropins; do
    [[ $path == "$nftables_dropin" ]] || die "unmanaged nftables.service drop-in requires manual review: $path"
  done
}

[[ $(uname -s) == Linux ]] || die 'run on Linux'
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ $repo_root =~ ^/[A-Za-z0-9_./-]+$ ]] || die 'repository path contains unsupported unit-file characters'
if [[ $# == 1 && $1 == --check-workspace-sync-mount-bindings ]]; then
  check_workspace_sync_mount_bindings
  exit 0
fi
[[ $# == 1 ]] || die 'usage: sudo deploy/single/install-portal-service.sh <existing-portal-user>'
[[ $EUID == 0 ]] || die 'run as root on Linux'
command -v systemctl >/dev/null && command -v runuser >/dev/null || die 'systemd and runuser are required'

unit_dir=/etc/systemd/system
nftables_dropin_dir=$unit_dir/nftables.service.d
nftables_dropin=$nftables_dropin_dir/50-morde-workspace-sync.conf
validate_nftables_base_unit

portal_user=$1
[[ $portal_user =~ ^[a-z_][a-z0-9_-]*$ ]] || die 'invalid service account name'
portal_uid=$(id -u "$portal_user" 2>/dev/null) || die 'service account does not exist'
((portal_uid != 0)) || die 'service account must not be root'
case "$repo_root/" in /root/*|/home/*|/run/user/*) die 'move the checkout outside protected home directories' ;; esac

env_file=/etc/cloudhost/cloudhost.env
[[ -f $env_file && ! -L $env_file && $(stat -c '%u:%a' "$env_file") == 0:600 ]] || die 'environment file must be root-owned, regular and mode 0600'
env_values=()
while IFS= read -r -d '' value; do env_values+=("$value"); done < <(
  python3 "$repo_root/deploy/single/read-env-values.py" --null "$env_file" \
    CH_DATA_KEY_FILE CH_COOKIE_KEY_FILE CH_WORKSPACE_STORAGE_ROOT_DIR \
    CH_SITE_STORAGE_ROOT_DIR CH_DOCKER_DATA_DIR CH_POSTGRES_DATA_DIR \
    CH_WORKSPACE_SYNC_CURRENT_FILES_DIR CH_DOCKER_HOST CH_OSB_URL CH_OSB_CONFIG_FILE
)
((${#env_values[@]} == 10)) || die 'could not read the required non-secret environment values'
CH_DATA_KEY_FILE=${env_values[0]}
CH_COOKIE_KEY_FILE=${env_values[1]}
CH_WORKSPACE_STORAGE_ROOT_DIR=${env_values[2]}
CH_SITE_STORAGE_ROOT_DIR=${env_values[3]}
CH_DOCKER_DATA_DIR=${env_values[4]}
CH_POSTGRES_DATA_DIR=${env_values[5]}
CH_WORKSPACE_SYNC_CURRENT_FILES_DIR=${env_values[6]}
CH_DOCKER_HOST=${env_values[7]}
CH_OSB_URL=${env_values[8]}
CH_OSB_CONFIG_FILE=${env_values[9]}

workspace_sync_enabled=$(workspace_sync_enabled_from_config)
[[ $workspace_sync_enabled == true || $workspace_sync_enabled == false ]] || die 'workspace_sync_node.enabled must be a literal true or false'
check_workspace_sync_current_root_config
sync_roots=("$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR")
if [[ $workspace_sync_enabled == true ]]; then
  sync_values=()
  while IFS= read -r -d '' value; do sync_values+=("$value"); done < <(
    python3 "$repo_root/deploy/single/read-env-values.py" --null "$env_file" \
      CH_WORKSPACE_SYNC_NODE_STATE_DIR CH_WORKSPACE_SYNC_RELAY_STATE_DIR
  )
  ((${#sync_values[@]} == 2)) || die 'could not read the Workspace sync data roots'
  sync_roots=("$CH_WORKSPACE_STORAGE_ROOT_DIR" "$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR" "${sync_values[0]}" "${sync_values[1]}")
fi
workspace_sync_mount_units "${sync_roots[@]}"
current_device=$(stat -c '%d' "$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR")
for path in / "$CH_WORKSPACE_STORAGE_ROOT_DIR" "$CH_DOCKER_DATA_DIR" "$CH_SITE_STORAGE_ROOT_DIR" "$CH_POSTGRES_DATA_DIR"; do
  [[ -n $path && -d $path ]] || die "Workspace current-files isolation path is unavailable: $path"
  [[ $(stat -c '%d' "$path") != "$current_device" ]] || die "Workspace current-files root must use a filesystem separate from $path"
done
sync_mount_units_text=${sync_mount_units[*]:-}
sync_mount_binding=
if ((${#sync_mount_units[@]})); then
  sync_mount_binding="BindsTo=$sync_mount_units_text"
fi

docker_socket=${CH_DOCKER_HOST#unix://}
[[ $CH_DOCKER_HOST == unix:///* && -S $docker_socket ]] || die 'CH_DOCKER_HOST must point to a local Unix Docker socket'

for key in CH_DATA_KEY_FILE CH_COOKIE_KEY_FILE; do
  file=${!key:-}
  [[ $file == /* && -f $file && ! -L $file && $(stat -c '%u:%a:%s' "$file") == "$portal_uid:600:32" ]] || die "$key must be an absolute 32-byte mode 0600 file owned by $portal_user"
done
for key in CH_WORKSPACE_STORAGE_ROOT_DIR CH_SITE_STORAGE_ROOT_DIR CH_WORKSPACE_SYNC_CURRENT_FILES_DIR; do
  path=${!key:-}
  [[ $path == /* && -d $path && ! -L $path && $(stat -c '%u:%a' "$path") == "$portal_uid:700" ]] || die "$key must be a mode 0700 directory owned by $portal_user"
  runuser -u "$portal_user" -- test -w "$path" || die "$portal_user cannot write $key"
done
for path in "$repo_root" "$repo_root/deploy/single/portal.yaml" "$repo_root/web/portal/dist/index.html" "$repo_root/web/portal/dist/admin/index.html" "$repo_root/web/portal/dist/app/index.html" "$repo_root/web/workbench/dist/index.html"; do
  runuser -u "$portal_user" -- test -r "$path" || die "$portal_user cannot read $path"
done
for path in "$repo_root" "$repo_root/web/portal/dist" "$repo_root/web/portal/dist/app" "$repo_root/web/workbench/dist"; do
  runuser -u "$portal_user" -- test -x "$path" || die "$portal_user cannot traverse $path"
done
runuser -u "$portal_user" -- test -x "$repo_root/.local/bin/portal" || die 'Portal binary is missing or not executable by the service account'
python3 "$repo_root/deploy/single/validate-portal-binary.py" "$repo_root/.local/bin/portal" "$(uname -m)" || die 'Portal binary is not a Linux executable for this host architecture'
quota_socket_root=/var/lib/morde-quota
if [[ ! -e $quota_socket_root && ! -L $quota_socket_root ]]; then
  install -d -o "$portal_user" -g "$(id -gn "$portal_user")" -m 0700 "$quota_socket_root"
fi
[[ -d $quota_socket_root && ! -L $quota_socket_root && $(stat -c '%u:%a' "$quota_socket_root") == "$portal_uid:700" ]] || die "$quota_socket_root must be a mode 0700 directory owned by $portal_user"
runuser -u "$portal_user" -- test -w "$quota_socket_root" || die "$portal_user cannot write the Workspace sync quota socket root"
policy=/opt/cloudhost/network-policy.sh
[[ -d /opt/cloudhost && ! -L /opt/cloudhost && $(stat -c '%u' /opt/cloudhost) == 0 ]] || die '/opt/cloudhost must be a root-owned directory'
[[ -f $policy && ! -L $policy && -x $policy && $(stat -c '%u' "$policy") == 0 ]] || die 'install the root-owned network policy script first'
for path in /opt/cloudhost "$policy"; do
  mode=$(stat -c '%a' "$path")
  (( (8#$mode & 8#022) == 0 )) || die "$path must not be writable by group or others"
done
sudoers_dir=/etc/sudoers.d
sudoers_file=$sudoers_dir/morde-network-policy
[[ -d $sudoers_dir && ! -L $sudoers_dir && $(stat -c '%u' "$sudoers_dir") == 0 ]] || die '/etc/sudoers.d must be a root-owned directory'
if [[ -e $sudoers_file ]] && { [[ ! -f $sudoers_file || -L $sudoers_file ]] || ! head -n 1 "$sudoers_file" | grep -Fxq '# Generated by deploy/single/install-portal-service.sh'; }; then
  die 'an unmanaged morde-network-policy sudoers file already exists'
fi
command -v visudo >/dev/null 2>&1 || die 'visudo is required to install the narrow network-policy permissions'
temp_sudoers=$(mktemp "$sudoers_dir/.morde-network-policy.XXXXXX")
trap 'rm -f -- "$temp_sudoers"' EXIT
cat >"$temp_sudoers" <<EOF
# Generated by deploy/single/install-portal-service.sh
Cmnd_Alias MORDE_NETWORK_POLICY_READ = /usr/sbin/nft -j -s -nn list table inet cloudhost, /usr/sbin/nft -j -s -nn list table bridge cloudhost
Cmnd_Alias MORDE_SYNC_RELAY_GATE = /opt/cloudhost/network-policy.sh relay-enable-stdin, /opt/cloudhost/network-policy.sh relay-disable-stdin, /opt/cloudhost/network-policy.sh relay-disable-all
$portal_user ALL=(root) NOPASSWD: MORDE_NETWORK_POLICY_READ, MORDE_SYNC_RELAY_GATE
EOF
chmod 0440 "$temp_sudoers"
visudo -cf "$temp_sudoers" >/dev/null || die 'generated network-policy sudoers permissions are invalid'
chown root:root "$temp_sudoers"
mv -f -- "$temp_sudoers" "$sudoers_file"
temp_sudoers=''
trap - EXIT
portal_unit=$unit_dir/morde-portal.service
network_unit=$unit_dir/morde-network-policy.service
preload_unit=$unit_dir/morde-network-preload.service
docker_dropin_dir=$unit_dir/docker.service.d
docker_dropin=$docker_dropin_dir/50-morde-network-preload.conf
[[ ! -L $portal_unit && ! -L $network_unit && ! -L $preload_unit && ! -L $docker_dropin_dir && ! -L $docker_dropin && ! -L $nftables_dropin_dir && ! -L $nftables_dropin ]] || die 'refusing symlinked systemd paths'
[[ ! -e $docker_dropin_dir || -d $docker_dropin_dir ]] || die 'Docker drop-in path is not a directory'
[[ ! -e $nftables_dropin_dir || -d $nftables_dropin_dir ]] || die 'nftables drop-in path is not a directory'
if [[ -e $portal_unit ]] && ! head -n 1 "$portal_unit" | grep -Fxq '# Generated by deploy/single/install-portal-service.sh'; then
  die 'an unmanaged morde-portal.service already exists'
fi
if [[ -e $network_unit ]] && ! cmp -s "$repo_root/deploy/single/morde-network-policy.service" "$network_unit"; then
  die 'a different morde-network-policy.service already exists'
fi
if [[ -e $preload_unit ]] && ! cmp -s "$repo_root/deploy/single/morde-network-preload.service" "$preload_unit"; then
  die 'a different morde-network-preload.service already exists'
fi
if [[ -e $docker_dropin ]] && ! cmp -s "$repo_root/deploy/single/docker-network-preload.conf" "$docker_dropin"; then
  die 'a different Docker preload drop-in already exists'
fi
if [[ -e $nftables_dropin ]] && ! cmp -s "$repo_root/deploy/single/nftables-workspace-sync.conf" "$nftables_dropin"; then
  die 'a different nftables lifecycle drop-in already exists'
fi
install -d -o root -g root -m 0755 "$docker_dropin_dir"
install -d -o root -g root -m 0755 "$nftables_dropin_dir"
install -o root -g root -m 0644 "$repo_root/deploy/single/docker-network-preload.conf" "$docker_dropin"
install -o root -g root -m 0644 "$repo_root/deploy/single/nftables-workspace-sync.conf" "$nftables_dropin"
install -o root -g root -m 0644 "$repo_root/deploy/single/morde-network-preload.service" "$preload_unit"
install -o root -g root -m 0644 "$repo_root/deploy/single/morde-network-policy.service" "$network_unit"
temp_unit=$(mktemp "$unit_dir/.morde-portal.service.XXXXXX")
trap 'rm -f -- "$temp_unit"' EXIT
cat >"$temp_unit" <<EOF
# Generated by deploy/single/install-portal-service.sh
[Unit]
Description=Morde Portal
Requires=docker.service morde-network-policy.service
$sync_mount_binding
PartOf=morde-network-policy.service nftables.service
After=network-online.target docker.service morde-network-policy.service nftables.service $sync_mount_units_text
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$portal_user
SupplementaryGroups=65532
EnvironmentFile=/etc/cloudhost/cloudhost.env
WorkingDirectory=$repo_root
ExecStartPre=/opt/cloudhost/network-policy.sh check
ExecStartPre=/bin/bash $repo_root/deploy/single/install-portal-service.sh --check-workspace-sync-mount-bindings
ExecStart=$repo_root/.local/bin/portal serve --config $repo_root/deploy/single/portal.yaml
Restart=always
RestartSec=10s
TimeoutStopSec=45s
UMask=0077
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$temp_unit"
mv -f -- "$temp_unit" "$portal_unit"
systemctl daemon-reload
systemctl enable morde-network-preload.service morde-network-policy.service morde-portal.service
systemctl stop morde-portal.service
systemctl reload-or-restart nftables.service
systemctl reload-or-restart morde-network-preload.service
systemctl reload-or-restart morde-network-policy.service
systemctl restart morde-portal.service
systemctl is-active --quiet morde-portal.service || die 'Portal service did not start; inspect journalctl -u morde-portal.service'
printf 'Portal service installed for %s from %s\n' "$portal_user" "$repo_root"
