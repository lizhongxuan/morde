#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc "$root/Shared/MinisStoragePaths.swift" "$root/Tests/MinisStoragePathsTests.swift" -o "$temp/minis-storage-paths-tests"
"$temp/minis-storage-paths-tests"
