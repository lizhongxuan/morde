#!/usr/bin/env bash

set -Eeuo pipefail

# shellcheck source=deploy/single/verify/lib.sh
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
verify_begin

VERIFY_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
M0_DIR="$(cd -- "$VERIFY_DIR/../m0" && pwd)"

report_host_facts() {
  printf 'INFO A01.01 host facts begin\n'
  if [[ -r /etc/os-release ]]; then
    awk -F= '/^(PRETTY_NAME|VERSION_ID)=/ {gsub(/^"|"$/, "", $2); print "INFO A01.01 " $1 "=" $2}' /etc/os-release
  fi
  printf 'INFO A01.01 kernel=%s\n' "$(uname -sr)"
  if command -v lscpu >/dev/null 2>&1; then
    lscpu | awk -F: '/^Model name:/ {gsub(/^[[:space:]]+/, "", $2); print "INFO A01.01 cpu_model=" $2; exit}'
  fi
  if command -v nproc >/dev/null 2>&1; then
    printf 'INFO A01.01 cpu_count=%s\n' "$(nproc)"
  fi
  if command -v free >/dev/null 2>&1; then
    free -h | awk 'NR == 2 {print "INFO A01.01 memory=" $2}'
  fi
  if [[ -e /sys/fs/cgroup/cgroup.controllers ]]; then
    printf 'INFO A01.01 cgroup_version=v2\n'
  elif [[ -d /sys/fs/cgroup/pids ]]; then
    printf 'INFO A01.01 cgroup_version=v1\n'
  else
    printf 'INFO A01.01 cgroup_version=unknown\n'
  fi
  if command -v docker >/dev/null 2>&1; then
    printf 'INFO A01.01 docker=%s\n' "$(docker --version 2>/dev/null || printf unavailable)"
    docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
    if [[ -n "$docker_root" ]]; then
      printf 'INFO A01.01 docker_data_root=%s\n' "$docker_root"
      df -h "$docker_root" 2>/dev/null | awk 'NR == 2 {print "INFO A01.01 docker_filesystem=" $1 " available=" $4 " mount=" $6}' || true
    fi
  fi
  if [[ -d /var/log/journal ]] && command -v df >/dev/null 2>&1; then
    df -h /var/log/journal | awk 'NR == 2 {print "INFO A01.01 journal_filesystem=" $2 " available=" $4 " mount=" $6}'
  else
    printf 'INFO A01.01 journal_filesystem=not-present-or-not-mounted\n'
  fi
  printf 'INFO A01.01 record the CPU model, exact memory, Docker data-root filesystem, and PostgreSQL data filesystem in the verification record\n'
}

report_host_facts

check_runsc() {
  command -v docker >/dev/null 2>&1 && command -v runsc >/dev/null 2>&1 || return 1
  docker info --format '{{json .Runtimes}}' | jq -e 'has("runsc")' >/dev/null || return 1
  runsc --version >/dev/null 2>&1
}

check A01.02 'Docker registers runsc and runsc --version succeeds' check_runsc || true

verify_run_script A01.03 'runc AIO shell, screenshot, and VNC surface' "$M0_DIR/01-runc.sh" || true
verify_run_script A01.04 'runsc AIO shell, browser, screenshot, and VNC smoke check' "$M0_DIR/02-gvisor.sh" || true
verify_run_script A01.05 'AIO API-key authorization behavior' "$M0_DIR/07-aio-surface.sh" || true
verify_run_script A01.07 'OpenSandbox create, ready, screenshot, delete lifecycle' "$M0_DIR/03-osb.sh" || true
verify_run_script A01.08 'named-volume persistence and gem write access' "$M0_DIR/04-volume.sh" || true
verify_run_script A01.10 'cloudhost bridge isolation, public egress, private/metadata blocking, and raw-socket denial' "$M0_DIR/05-egress.sh" || true

printf 'INFO A01.06 terminal URL/download path and /dev/shm observations are emitted by 07-aio-surface.sh\n'
printf 'INFO A01.09 disk-full test intentionally omitted; verify only on a disposable filesystem and record Docker/PostgreSQL partition separation\n'

verify_finish
