#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
sed -n '/^\/\/ BEGIN Morde host navigation helpers$/,/^\/\/ END Morde host navigation helpers$/p' \
    "$root/Morde/MordeApp.swift" | sed '1d;$d' > "$temp/MordeHostNavigation.swift"
for mode in debug release; do
    if [ "$mode" = debug ]; then
        swiftc -D DEBUG "$temp/MordeHostNavigation.swift" "$root/Tests/MordeHostNavigationTests.swift" -o "$temp/morde-host-navigation-tests-$mode"
    else
        swiftc "$temp/MordeHostNavigation.swift" "$root/Tests/MordeHostNavigationTests.swift" -o "$temp/morde-host-navigation-tests-$mode"
    fi
    "$temp/morde-host-navigation-tests-$mode"
done
