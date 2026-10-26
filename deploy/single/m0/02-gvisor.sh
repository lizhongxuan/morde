#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-02-gvisor
m0_require_linux_docker
m0_require_pinned_aio_image

m0_start_aio_container runsc
m0_aio_check_surface "$CH_AIO_URL" A01.04
m0_note "runsc stats: $(docker stats --no-stream --format '{{.CPUPerc}} CPU, {{.MemUsage}} memory' "$M0_CONTAINER_NAME")"
m0_finish
