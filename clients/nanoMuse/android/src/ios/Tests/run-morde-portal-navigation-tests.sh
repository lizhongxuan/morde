#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
for mode in release debug; do
    if [ "$mode" = debug ]; then
        swiftc -D DEBUG "$root/Morde/MordeCredentialExpiry.swift" "$root/Morde/MordePortalNavigation.swift" "$root/Tests/MordePortalNavigationTests.swift" -o "$temp/morde-portal-navigation-tests"
    else
        swiftc "$root/Morde/MordeCredentialExpiry.swift" "$root/Morde/MordePortalNavigation.swift" "$root/Tests/MordePortalNavigationTests.swift" -o "$temp/morde-portal-navigation-tests"
    fi
    "$temp/morde-portal-navigation-tests"
done
