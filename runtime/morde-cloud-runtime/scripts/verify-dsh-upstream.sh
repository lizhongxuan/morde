#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
runtime_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
repo_root=$(CDPATH= cd -- "$runtime_dir/../.." && pwd)
dsh_root=${DSH_CHECKOUT:-"$repo_root/project/deepseek-harness"}
commit=fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159
release_commit=66491b05e3f05ad35bcdd4f6cd7ed26041d9e3db
package=packages/compaction/compaction-basic
vendored="$runtime_dir/vendor/deepseek-harness/$package"

git -C "$dsh_root" cat-file -e "$commit^{commit}"
git -C "$dsh_root" diff --exit-code "$release_commit" "$commit" -- "$package/package.json" "$package/src" >/dev/null

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/morde-dsh-upstream.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 HUP INT TERM
git -C "$dsh_root" show "$commit:$package/package.json" > "$tmp_dir/package.json"
cmp "$vendored/package.json" "$tmp_dir/package.json"

upstream_files=$(git -C "$dsh_root" ls-tree -r --name-only "$commit" -- "$package/src" | sed "s,^$package/src/,," | LC_ALL=C sort)
vendored_files=$(find "$vendored/src" -type f | sed "s,^$vendored/src/,," | LC_ALL=C sort)
if [ "$upstream_files" != "$vendored_files" ]; then
  echo "vendored DSH source file list differs from $commit" >&2
  exit 1
fi

while IFS= read -r path; do
  [ -n "$path" ] || continue
  git -C "$dsh_root" show "$commit:$package/src/$path" > "$tmp_dir/source"
  cmp "$vendored/src/$path" "$tmp_dir/source"
done <<EOF
$upstream_files
EOF

echo "DSH source matches $commit (release $release_commit)"
