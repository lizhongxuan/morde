#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
source_dir="${SYNCTHING_SOURCE_DIR:-}"
image_platform="${IMAGE_PLATFORM:-linux/amd64}"

if [[ -z "$source_dir" || ! -f "$source_dir/go.mod" || ! -f "$source_dir/cmd/syncthing/main.go" || ! -f "$source_dir/cmd/strelaysrv/main.go" || ! -f "$source_dir/LICENSE" ]]; then
    printf 'Set SYNCTHING_SOURCE_DIR to the complete local Syncthing source worktree.\n' >&2
    exit 1
fi
source_dir="$(cd "$source_dir" && pwd)"

case "$image_platform" in
    linux/amd64) image_arch=amd64 ;;
    linux/arm64) image_arch=arm64 ;;
    *)
        printf 'Unsupported IMAGE_PLATFORM: %s (allowed: linux/amd64, linux/arm64)\n' "$image_platform" >&2
        exit 2
        ;;
esac

if ! command -v docker >/dev/null 2>&1 || ! command -v go >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1; then
    printf 'Docker, Go, and Git must be installed locally.\n' >&2
    exit 1
fi

if [[ -n "${DOCKER_HOST:-}" ]]; then
    docker_endpoint="$DOCKER_HOST"
else
    docker_context="$(docker context show)"
    docker_endpoint="$(docker context inspect "$docker_context" --format '{{ (index .Endpoints "docker").Host }}')"
fi
if [[ "$docker_endpoint" != unix://* ]]; then
    printf 'Refusing to send build context to non-local Docker endpoint: %s\n' "$docker_endpoint" >&2
    exit 1
fi

source_revision="$(git -C "$source_dir" rev-parse HEAD)"
sha256_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    else
        shasum -a 256 | awk '{print $1}'
    fi
}
source_patch_sha256="$(
    {
        git -C "$source_dir" diff --binary HEAD
        while IFS= read -r untracked_path; do
            printf 'untracked path: %s\n' "$untracked_path"
            if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "$source_dir/$untracked_path"
            else
                shasum -a 256 "$source_dir/$untracked_path"
            fi
        done < <(git -C "$source_dir" ls-files --others --exclude-standard -- . ':!.morde-local/**')
    } | sha256_stream
)"
source_key="${source_revision:0:12}-${source_patch_sha256:0:12}-${image_arch}"
local_go_version="$(GOTOOLCHAIN=local go env GOVERSION)"
node_tag="${WORKSPACE_SYNC_NODE_IMAGE_TAG:-local/morde-workspace-sync-node:$source_key}"
relay_tag="${WORKSPACE_SYNC_RELAY_IMAGE_TAG:-local/morde-workspace-sync-relay:$source_key}"
build_dir="$(mktemp -d "${TMPDIR:-/tmp}/morde-workspace-sync-images.XXXXXX")"
trap 'rm -rf "$build_dir"' EXIT

mkdir -p "$build_dir/node" "$build_dir/relay"
cp "$source_dir/LICENSE" "$build_dir/node/LICENSE"
cp "$source_dir/LICENSE" "$build_dir/relay/LICENSE"

GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0 GOOS=linux GOARCH="$image_arch" go -C "$source_dir" build \
    -trimpath -ldflags='-s -w' -o "$build_dir/node/syncthing" ./cmd/syncthing
GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0 GOOS=linux GOARCH="$image_arch" go -C "$source_dir" build \
    -trimpath -ldflags='-s -w' -o "$build_dir/relay/strelaysrv" ./cmd/strelaysrv

for image_kind in node relay; do
    if [[ "$image_kind" == node ]]; then
        image_tag="$node_tag"
        dockerfile="$script_dir/node.Dockerfile"
    else
        image_tag="$relay_tag"
        dockerfile="$script_dir/relay.Dockerfile"
    fi
    DOCKER_BUILDKIT=1 docker build --platform "$image_platform" \
        --label "org.opencontainers.image.revision=$source_revision" \
        --label "org.morde.syncthing.patch-sha256=$source_patch_sha256" \
        --label "org.morde.syncthing.go-version=$local_go_version" \
        --tag "$image_tag" --file "$dockerfile" "$build_dir/$image_kind"
done

node_image_id="$(docker image inspect --format '{{.Id}}' "$node_tag")"
relay_image_id="$(docker image inspect --format '{{.Id}}' "$relay_tag")"
printf 'Platform: %s\nSyncthing revision: %s\nLocal Go toolchain: %s\nPatch SHA-256: %s\nNode tag: %s\nNode content ID: %s\nRelay tag: %s\nRelay content ID: %s\n' \
    "$image_platform" "$source_revision" "$local_go_version" "$source_patch_sha256" "$node_tag" "$node_image_id" "$relay_tag" "$relay_image_id"
