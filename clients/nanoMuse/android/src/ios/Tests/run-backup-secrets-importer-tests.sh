#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temp=$(mktemp -d)
trap 'rm -rf "$temp"' EXIT
swiftc \
  "$root/Morde/MordeCredentialExpiry.swift" \
  "$root/Agent/Backup/BackupSecretsImporter.swift" \
  "$root/Tests/BackupSecretsImporterTests.swift" \
  -o "$temp/backup-secrets-importer-tests"
"$temp/backup-secrets-importer-tests"
