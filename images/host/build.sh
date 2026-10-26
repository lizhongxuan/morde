#!/usr/bin/env bash
set -euo pipefail

image_tag="${IMAGE_TAG:-registry.local/cloudhost/host:1.0.0}"
image_platform="${IMAGE_PLATFORM:-linux/amd64}"
case "$image_platform" in
    linux/amd64) runtime_arch=amd64 ;;
    linux/arm64) runtime_arch=arm64 ;;
    *)
        printf 'Unsupported IMAGE_PLATFORM: %s (allowed: linux/amd64, linux/arm64)\n' "$image_platform" >&2
        exit 2
        ;;
esac
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project_dir="$(cd "$script_dir/../.." && pwd)"
runtime_context="$project_dir/project/morde-runtime"
if [[ ! -f "$runtime_context/go.mod" || ! -f "$runtime_context/cmd/morde-runtime/main.go" ]]; then
    printf 'morde-runtime source is incomplete: %s\n' "$runtime_context" >&2
    exit 1
fi
required_go_version="$(sed -n 's/^toolchain[[:space:]]*//p' "$runtime_context/go.mod")"
if [[ -z "$required_go_version" ]]; then
	printf 'morde-runtime go.mod must pin its build toolchain\n' >&2
	exit 1
fi
local_go_version="$(GOTOOLCHAIN=local go env GOVERSION 2>/dev/null || true)"
if [[ "$local_go_version" != "$required_go_version" ]]; then
	printf 'Go toolchain mismatch: need %s, found %s\n' "$required_go_version" "${local_go_version:-unavailable}" >&2
	exit 1
fi
build_dir="$(mktemp -d "${TMPDIR:-/tmp}/cloudhost-runtime.XXXXXX")"
metadata_file="$(mktemp "${TMPDIR:-/tmp}/cloudhost-host-image.XXXXXX")"
trap 'rm "$metadata_file"; rm -r "$build_dir"' EXIT

GOTOOLCHAIN=local GOOS=linux GOARCH="$runtime_arch" CGO_ENABLED=0 go -C "$runtime_context" build \
    -trimpath -ldflags='-s -w' -o "$build_dir/morde-runtime" ./cmd/morde-runtime

docker buildx build \
    --file "$script_dir/Dockerfile" \
    --build-context "runtime=$build_dir" \
    --platform "$image_platform" \
    --load \
    --metadata-file "$metadata_file" \
    --tag "$image_tag" \
    "$script_dir"

image_id="$(docker image inspect --platform "$image_platform" --format '{{.Id}}' "$image_tag")"
digest="$(sed -n 's/.*"containerimage.digest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$metadata_file" | head -n 1)"
if [[ -z "$digest" ]]; then
    printf 'Build succeeded but Buildx did not report a manifest digest. Local image ID: %s\n' "$image_id" >&2
    exit 1
fi

printf 'Image: %s\nPlatform: %s\nImage ID: %s\nDigest: %s\n' "$image_tag" "$image_platform" "$image_id" "$digest"
