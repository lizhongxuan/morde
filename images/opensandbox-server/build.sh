#!/usr/bin/env bash
set -euo pipefail

image_tag="${IMAGE_TAG:-registry.local/cloudhost/opensandbox-server:release-1.1.1-runtime-8090}"
image_platform="${IMAGE_PLATFORM:-linux/amd64}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker buildx build \
    --file "$script_dir/Dockerfile" \
    --platform "$image_platform" \
    --load \
    --tag "$image_tag" \
    "$script_dir"

docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}} {{index .RepoTags 0}}' "$image_tag"
