#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc \
  "$root/Agent/Backup/LegacyRootfsArchive.swift" \
  "$root/Tests/LegacyRootfsArchiveTests.swift" \
  -o "$temp/legacy-rootfs-archive-tests"
"$temp/legacy-rootfs-archive-tests"
