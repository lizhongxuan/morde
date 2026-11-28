#!/usr/bin/env bash
set -Eeuo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE='ghcr.io/searxng/searxng:2026.11.5-3e95f6cd2@sha256:ffad724acd87b8dd312328d18da88ddd42e5c4b1d23231594662620053c13cae'
ENGINE_PATH="$REPO_ROOT/deploy/single/search/morde_360_news.py"
TEST_PATH="$REPO_ROOT/deploy/single/search/test/test_morde_360_news.py"

if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  printf 'Pinned SearXNG image is not present locally; run scripts/setup-builtin-search.sh before this offline test.\n' >&2
  exit 2
fi

docker run --rm --network none --read-only --tmpfs /tmp:rw,noexec,nosuid,size=16m --cap-drop ALL --security-opt no-new-privileges \
  -e PYTHONDONTWRITEBYTECODE=1 \
  -e PYTHONPATH=/usr/local/searxng \
  -v "$ENGINE_PATH:/tmp/morde_360_news.py:ro" \
  -v "$TEST_PATH:/tmp/test_morde_360_news.py:ro" \
  --entrypoint /usr/local/searxng/.venv/bin/python3 "$IMAGE" /tmp/test_morde_360_news.py
