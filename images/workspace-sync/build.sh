#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
source_dir="${SYNCTHING_SOURCE_DIR:-}"
image_platform="${IMAGE_PLATFORM:-linux/amd64}"
required_source_revision="ed38528d7fba3dd6a42c9c9fc6780af432dc15d2"
required_source_patch_sha256="0b34e1a0b635073f297cb844f27d2d93fc798b744cddbdd3828f490e5d29f3e8"
required_go_version="go1.27.1"

if [[ -z "$source_dir" || ! -f "$source_dir/go.mod" || ! -f "$source_dir/cmd/syncthing/main.go" || ! -f "$source_dir/cmd/strelaysrv/main.go" || ! -f "$source_dir/LICENSE" ]]; then
    printf 'Set SYNCTHING_SOURCE_DIR to the complete local Syncthing source worktree.\n' >&2
    exit 1
fi
source_dir="$(cd "$source_dir" && pwd)"

require_source_pattern() {
    local file="$1"
    local pattern="$2"
    if [[ ! -f "$source_dir/$file" ]] || ! grep -Fq -- "$pattern" "$source_dir/$file"; then
        printf 'Syncthing source is missing the required Morde quota hook: %s (%s)\n' "$file" "$pattern" >&2
        exit 1
    fi
}

require_source_pattern lib/model/morde_quota_client.go 'func (f *sendReceiveFolder) reserveMordeFile'
require_source_pattern lib/model/folder_sendrecv.go 'f.reserveMordeFile(ctx, file, "upsert", file.Size)'
require_source_pattern lib/model/folder_sendrecv.go 'f.reserveMordeFile(quotaCtx, file, "delete", 0)'
require_source_pattern lib/mordequota/quota.go 'package mordequota'
require_source_pattern lib/config/folderconfiguration.go 'MordeQuotaSocket'
require_source_pattern lib/config/folderconfiguration.go 'MordeWorkspaceID'
require_source_pattern lib/config/folderconfiguration.go 'MordeMaxSingleFileBytes'

while IFS= read -r -d '' ignored_path; do
    case "$ignored_path" in
        *.go)
            printf 'Refusing ignored Go source that is not covered by the Syncthing patch digest: %s\n' "$ignored_path" >&2
            exit 1
            ;;
    esac
done < <(git -C "$source_dir" ls-files --others --ignored --exclude-standard -z)

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
            sha256_stream < "$source_dir/$untracked_path"
        done < <(git -C "$source_dir" ls-files --others --exclude-standard -- . ':!.morde-local/**' | LC_ALL=C sort)
    } | sha256_stream
)"
if [[ "$source_revision" != "$required_source_revision" || "$source_patch_sha256" != "$required_source_patch_sha256" ]]; then
    printf 'Unsupported Syncthing source or Morde patch. Expected revision %s and patch SHA-256 %s.\n' \
        "$required_source_revision" "$required_source_patch_sha256" >&2
    exit 1
fi
source_key="${source_revision:0:12}-${source_patch_sha256:0:12}-${image_arch}"
local_go_version="$(GOTOOLCHAIN=local go env GOVERSION)"
if [[ "$local_go_version" != "$required_go_version" ]]; then
    printf 'Syncthing image requires Go %s; found %s.\n' "$required_go_version" "$local_go_version" >&2
    exit 1
fi
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
    image_labels=(
        --label "org.opencontainers.image.revision=$source_revision"
        --label "org.morde.syncthing.patch-sha256=$source_patch_sha256"
        --label "org.morde.syncthing.go-version=$local_go_version"
    )
    if [[ "$image_kind" == node ]]; then
        image_tag="$node_tag"
        dockerfile="$script_dir/node.Dockerfile"
        image_labels+=(--label "org.morde.syncthing.workspace-quota-hook=f02-v1")
    else
        image_tag="$relay_tag"
        dockerfile="$script_dir/relay.Dockerfile"
    fi
    DOCKER_BUILDKIT=1 docker build --platform "$image_platform" \
        "${image_labels[@]}" \
        --tag "$image_tag" --file "$dockerfile" "$build_dir/$image_kind"
done

node_image_id="$(docker image inspect --format '{{.Id}}' "$node_tag")"
relay_image_id="$(docker image inspect --format '{{.Id}}' "$relay_tag")"
printf 'Platform: %s\nSyncthing revision: %s\nLocal Go toolchain: %s\nPatch SHA-256: %s\nNode tag: %s\nNode content ID: %s\nRelay tag: %s\nRelay content ID: %s\n' \
    "$image_platform" "$source_revision" "$local_go_version" "$source_patch_sha256" "$node_tag" "$node_image_id" "$relay_tag" "$relay_image_id"
