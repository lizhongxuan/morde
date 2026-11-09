#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc "$root/Morde/MordeWorkspaceTransfer.swift" "$root/Tests/MordeWorkspaceTransferTests.swift" -o "$temp/morde-workspace-transfer-tests"
"$temp/morde-workspace-transfer-tests"
