#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/m0/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
m0_begin m0-01-runc
m0_require_linux_docker
m0_require_pinned_aio_image

m0_start_aio_container runc
m0_aio_check_surface "$CH_AIO_URL" A01.03
m0_pass 'A01.03 runc AIO shell, screenshot and VNC checks completed'
m0_finish
