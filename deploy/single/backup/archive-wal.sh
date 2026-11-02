#!/usr/bin/env bash
set -Eeuo pipefail

die() { printf 'WAL archive: %s\n' "$*" >&2; exit 1; }

[[ $# == 2 ]] || die 'expected a PostgreSQL WAL path and filename'
source_arg=$1
name=$2
[[ $name =~ ^([0-9A-F]{24}|[0-9A-F]{8}\.history|[0-9A-F]{24}\.[0-9A-F]{8}\.backup)$ ]] || die 'invalid PostgreSQL WAL filename'

pgdata=${PGDATA:-/var/lib/postgresql/data}
[[ $pgdata == /* && $pgdata != / && -d $pgdata && ! -L $pgdata ]] || die 'invalid PostgreSQL data directory'
pgdata=${pgdata%/}
[[ -d "$pgdata/pg_wal" && ! -L "$pgdata/pg_wal" ]] || die 'PostgreSQL WAL directory is unavailable'
case "$source_arg" in
  "pg_wal/$name") source_file="$pgdata/$source_arg" ;;
  "$pgdata/pg_wal/$name") source_file=$source_arg ;;
  *) die 'PostgreSQL WAL path does not match its filename' ;;
esac
[[ -f $source_file && ! -L $source_file ]] || die 'PostgreSQL WAL source is not a regular file'

archive_dir="${pgdata%/*}/wal-archive"
[[ -d $archive_dir && ! -L $archive_dir ]] || die 'WAL archive directory is unavailable'
destination="$archive_dir/$name"
if [[ -e $destination || -L $destination ]]; then
  [[ -f $destination && ! -L $destination ]] || die 'existing WAL archive is not a regular file'
  cmp -s "$source_file" "$destination" || die "existing WAL archive differs: $name"
  sync "$destination" && sync "$archive_dir" || die "could not sync existing WAL archive: $name"
  exit 0
fi

umask 077
tmp=$(mktemp "$archive_dir/.${name}.tmp.XXXXXX") || die 'could not create temporary WAL archive'
trap 'if [[ -n ${tmp:-} ]]; then rm -f -- "$tmp"; fi' EXIT
cp -- "$source_file" "$tmp" || die "could not copy WAL file: $name"
sync "$tmp" || die "could not sync temporary WAL archive: $name"
if ! mv --no-clobber --no-target-directory -- "$tmp" "$destination"; then
  [[ -f $destination && ! -L $destination ]] && cmp -s "$source_file" "$destination" || die "could not publish WAL archive: $name"
fi
if [[ -e $tmp ]]; then
  cmp -s "$source_file" "$destination" || die "existing WAL archive differs: $name"
  rm -f -- "$tmp"
fi
tmp=
cmp -s "$source_file" "$destination" || die "published WAL archive differs: $name"
sync "$archive_dir" || die "could not sync WAL archive directory: $name"
