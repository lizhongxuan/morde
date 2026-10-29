#!/usr/bin/env bash
set -Eeuo pipefail

die() { printf 'cloud-dsh render: %s\n' "$*" >&2; exit 1; }

[[ $# == 3 ]] || die 'usage: render-units.sh <portal-user> <dedicated-portal-group> <existing-output-directory>'
portal_user=$1
portal_group=$2
output_dir=$3
[[ $portal_user =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die 'invalid Linux user name'
[[ $portal_group =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die 'invalid Linux group name'
[[ $portal_group != root && $portal_group != docker ]] || die 'root and docker are not dedicated Portal groups'
[[ -d $output_dir && ! -L $output_dir ]] || die 'output directory must already exist and must not be a symlink'
output_dir=$(cd -- "$output_dir" && pwd -P)

command -v getent >/dev/null 2>&1 && command -v id >/dev/null 2>&1 || die 'getent and id are required for local account checks'
portal_passwd=$(getent -s files passwd "$portal_user") || die 'Portal user is not present in the local passwd database'
[[ -n $portal_passwd && $portal_passwd != *$'\n'* ]] || die 'local passwd entry is ambiguous'
IFS=: read -r passwd_name _ passwd_uid passwd_gid _ _ _ <<<"$portal_passwd"
[[ $passwd_name == "$portal_user" && $passwd_uid =~ ^[0-9]+$ && $passwd_gid =~ ^[0-9]+$ ]] || die 'invalid local passwd entry'
reported_uid=$(id -u "$portal_user") || die 'could not resolve the local Portal user UID'
[[ $reported_uid =~ ^[0-9]+$ && $reported_uid == "$passwd_uid" && $reported_uid != 0 ]] || die 'Portal user must exist locally and must not be root'

portal_group_entry=$(getent -s files group "$portal_group") || die 'Portal group is not present in the local group database'
[[ -n $portal_group_entry && $portal_group_entry != *$'\n'* ]] || die 'local group entry is ambiguous'
IFS=: read -r group_name _ group_gid group_members <<<"$portal_group_entry"
[[ $group_name == "$portal_group" && $group_gid =~ ^[0-9]+$ && $group_gid != 0 ]] || die 'invalid local Portal group entry'

local_passwd=$(getent -s files passwd) || die 'could not read the local passwd database'
local_passwd_user_count=0
while IFS=: read -r account _ _ primary_gid _; do
  [[ -n $account ]] || continue
  [[ $account != "$portal_user" ]] || local_passwd_user_count=$((local_passwd_user_count + 1))
  [[ $primary_gid != "$group_gid" || $account == "$portal_user" ]] || die "local account $account shares the Portal group's primary GID"
done <<<"$local_passwd"
[[ $local_passwd_user_count == 1 ]] || die 'Portal user must have exactly one local passwd entry'

local_groups=$(getent -s files group) || die 'could not read the local group database'
local_portal_group_count=0
while IFS=: read -r account_group _ account_gid _; do
  [[ -n $account_group ]] || continue
  if [[ $account_group == "$portal_group" ]]; then
    local_portal_group_count=$((local_portal_group_count + 1))
    [[ $account_gid == "$group_gid" ]] || die 'local Portal group entry is ambiguous'
  fi
  [[ $account_gid != "$group_gid" || $account_group == "$portal_group" ]] || die "local group $account_group shares the Portal group's GID"
done <<<"$local_groups"
[[ $local_portal_group_count == 1 ]] || die 'Portal group must have exactly one local group entry'

portal_group_member=false
if [[ $passwd_gid == "$group_gid" ]]; then portal_group_member=true; fi
if [[ -n $group_members ]]; then
  IFS=',' read -r -a listed_members <<<"$group_members"
  for member in "${listed_members[@]}"; do
    [[ $member == "$portal_user" ]] || die "Portal group lists another member: $member"
    portal_group_member=true
  done
fi
[[ $portal_group_member == true ]] || die 'Portal user must belong to the dedicated Portal group'

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
shopt -s nullglob
templates=("$script_dir"/*.service.in "$script_dir"/*.json.in)
((${#templates[@]} > 0)) || die 'no service or JSON templates found'

output_names=()
for template in "${templates[@]}"; do
  name=${template##*/}
  output_name=${name%.in}
  [[ ! -e $output_dir/$output_name && ! -L $output_dir/$output_name ]] || die "refusing to overwrite $output_dir/$output_name"
  while IFS= read -r placeholder; do
    [[ $placeholder == '@PORTAL_GROUP@' ]] || die "unknown placeholder $placeholder in $name"
  done < <(grep -Eo '@[^@[:space:]]+@' "$template" || true)
  output_names+=("$output_name")
done

stage_dir=$(mktemp -d "$output_dir/.render-units.XXXXXX")
created_outputs=()
cleanup() {
  status=$?
  if ((status != 0)); then
    for path in "${created_outputs[@]}"; do rm -f "$path" || true; done
  fi
  rm -rf "$stage_dir"
}
trap cleanup EXIT

for index in "${!templates[@]}"; do
  template=${templates[$index]}
  output_name=${output_names[$index]}
  sed "s/@PORTAL_GROUP@/$portal_group/g" "$template" >"$stage_dir/$output_name"
  if grep -Eq '@[^@[:space:]]+@' "$stage_dir/$output_name"; then
    die "unresolved placeholder in $output_name"
  fi
done

for output_name in "${output_names[@]}"; do
  target=$output_dir/$output_name
  ln "$stage_dir/$output_name" "$target" || die "could not create $target without overwriting an existing file"
  created_outputs+=("$target")
done

trap - EXIT
rm -rf "$stage_dir"
printf 'Rendered %d cloud-dsh files into %s for %s:%s\n' "${#output_names[@]}" "$output_dir" "$portal_user" "$portal_group"
