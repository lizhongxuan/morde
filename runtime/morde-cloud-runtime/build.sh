#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
platform=${CLOUD_DSH_PLATFORM:-linux/amd64}
image_tag=${CLOUD_DSH_IMAGE_TAG:-morde-cloud-runtime:local}
[[ $platform =~ ^linux/(amd64|arm64)$ ]] || { printf 'unsupported target platform: %s\n' "$platform" >&2; exit 2; }
[[ $image_tag =~ ^[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+$ ]] || { printf 'invalid local image tag\n' >&2; exit 2; }

check_base_digest() {
  local image=$1 expected=$2
  if ! docker image inspect --platform="$platform" "$image" >/dev/null 2>&1; then
    docker pull --platform="$platform" "$image"
  fi
  local actual_arch
  actual_arch=$(docker image inspect --platform="$platform" "$image" --format '{{.Architecture}}')
  [[ $actual_arch == "${platform#linux/}" ]] || { printf 'cached %s base is %s, not %s; select a matching target or cache the pinned target image first\n' "$image" "$actual_arch" "${platform#linux/}" >&2; exit 1; }
  python3 - "$platform" "$image" "$expected" <<'PY'
import json
import subprocess
import sys

image = json.loads(subprocess.check_output(
    ["docker", "image", "inspect", "--platform=" + sys.argv[1], sys.argv[2]], text=True
))[0]
expected = sys.argv[3]
if not any(value.endswith("@" + expected) for value in image.get("RepoDigests", [])):
    raise SystemExit(f"{sys.argv[2]} is not the required immutable base digest {expected}")
PY
}

check_base_digest node:24-slim@sha256:a5f4366e834695d5f363b920c104953a535d3a9864d1bdf74566a0ebe01daee5 sha256:a5f4366e834695d5f363b920c104953a535d3a9864d1bdf74566a0ebe01daee5
check_base_digest debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

metadata_file=$(mktemp)
trap 'rm -f -- "$metadata_file"' EXIT
docker buildx build \
  --platform "$platform" \
  --tag "$image_tag" \
  --metadata-file "$metadata_file" \
  --provenance=false \
  --load \
  "$script_dir"

python3 - "$metadata_file" "$image_tag" <<'PY'
import json
import subprocess
import sys

with open(sys.argv[1], encoding="utf-8") as metadata_file:
    metadata = json.load(metadata_file)
tag = sys.argv[2]
digest = metadata.get("containerimage.digest", "")
if not digest.startswith("sha256:") or len(digest) != 71:
    raise SystemExit("BuildKit did not return a manifest digest")

image = json.loads(subprocess.check_output(
    ["docker", "image", "inspect", tag], text=True
))[0]
if image.get("Config", {}).get("Env", []) != ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"]:
    raise SystemExit("runtime image environment is not the approved clean environment")
if image.get("Config", {}).get("User") != "65532:65532":
    raise SystemExit("runtime image does not default to the isolated non-root UID")
if image.get("Config", {}).get("Volumes"):
    raise SystemExit("runtime image must not declare writable volumes")

print(f"Local image: {tag}")
print(f"BuildKit manifest digest: {digest}")
print(f"Docker config/image ID: {image['Id']}")
if image.get("RepoDigests"):
    print("Repo digests: " + ", ".join(image["RepoDigests"]))
else:
    print("Repo digest: not assigned; this local image has not been published to a registry")
PY
