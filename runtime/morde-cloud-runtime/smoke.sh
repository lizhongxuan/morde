#!/usr/bin/env bash
set -Eeuo pipefail

image_ref=${CLOUD_DSH_IMAGE_REF:-morde-cloud-runtime:local}
platform=${CLOUD_DSH_PLATFORM:-linux/amd64}
[[ $platform =~ ^linux/(amd64|arm64)$ ]] || { printf 'unsupported target platform: %s\n' "$platform" >&2; exit 2; }
docker image inspect --platform="$platform" "$image_ref" >/dev/null || { printf 'runtime image is not cached locally for %s: %s\n' "$platform" "$image_ref" >&2; exit 1; }

version=$(docker run --rm \
  --platform="$platform" --network=none --read-only --user=65532:65532 --cap-drop=ALL \
  --security-opt=no-new-privileges:true --memory=256m --memory-swap=256m \
  --cpus=1 --pids-limit=64 --init \
  "$image_ref" /usr/bin/env -i PATH=/usr/bin:/bin LANG=C.UTF-8 NODE_ENV=production \
  /usr/local/bin/node --version)
[[ $version =~ ^v24\. ]] || { printf 'unexpected Node runtime version: %s\n' "$version" >&2; exit 1; }

output=$(printf '%s\n' '{"type":"start"}' | docker run --rm -i \
  --platform="$platform" --network=none --read-only --user=65532:65532 --cap-drop=ALL \
  --security-opt=no-new-privileges:true --memory=256m --memory-swap=256m \
  --cpus=1 --pids-limit=64 --init \
  "$image_ref" /usr/bin/env -i PATH=/usr/bin:/bin LANG=C.UTF-8 NODE_ENV=production \
  /usr/local/bin/node /opt/morde-cloud-runtime/bin.mjs 2>/dev/null) || status=$?
status=${status:-0}
[[ $status -ne 0 ]] || { printf 'invalid startup message unexpectedly succeeded\n' >&2; exit 1; }
printf '%s\n' "$output" | python3 -c '
import json, sys
events = [json.loads(line) for line in sys.stdin if line.strip()]
if not events or events[0].get("type") != "ready" or events[-1].get("type") != "failed":
    raise SystemExit("runtime did not emit its ready/fail-closed startup sequence")
'

account_output=$(docker run --rm \
  --platform="$platform" --network=none --read-only --user=65532:65532 --cap-drop=ALL \
  --security-opt=no-new-privileges:true --memory=256m --memory-swap=256m \
  --cpus=1 --pids-limit=64 --init \
  "$image_ref" /usr/bin/env -i PATH=/usr/bin:/bin LANG=C.UTF-8 NODE_ENV=production \
  /usr/local/bin/node /opt/morde-cloud-runtime/smoke-account-runtime.mjs 2>&1)
printf '%s\n' "$account_output"
printf 'runtime image smoke passed (%s, %s)\n' "$version" "$image_ref"
