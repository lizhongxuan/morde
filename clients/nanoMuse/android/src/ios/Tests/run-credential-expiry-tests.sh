#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
repo_root=$(CDPATH= cd -- "$root/../../../../.." && pwd)
fixture="$repo_root/server/openapi/fixtures/morde-client-v1.json"
test -f "$fixture"
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc "$root/Morde/MordeCredentialExpiry.swift" "$root/Tests/MordeCredentialExpiryTests.swift" -o "$temp/morde-credential-expiry-tests"
MORDE_CLIENT_CONTRACT_FIXTURE="$fixture" "$temp/morde-credential-expiry-tests"
swiftc -D DEBUG "$root/Morde/MordeCredentialExpiry.swift" "$root/Tests/MordeCredentialExpiryTests.swift" -o "$temp/morde-credential-expiry-debug-tests"
MORDE_CLIENT_CONTRACT_FIXTURE="$fixture" "$temp/morde-credential-expiry-debug-tests"
