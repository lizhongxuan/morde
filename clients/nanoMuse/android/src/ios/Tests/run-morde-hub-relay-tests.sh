#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc "$root/Morde/MordeHubRelay.swift" "$root/Tests/MordeHubRelayTests.swift" -o "$temp/morde-hub-relay-tests"
"$temp/morde-hub-relay-tests"
