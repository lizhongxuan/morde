#!/usr/bin/env bash
# morde-update.sh — 从 lizhongxuan/morde 更新阿里云上的 Morde Portal
#
# 流程：拉最新代码 → 构建新版 portal → 校验二进制 → 迁移状态检查
#       → 备份当前版 → 跑数据库迁移 → 原子替换二进制 → 重启服务
#       → 健康检查 → 失败自动回滚到备份版。
#
# 安全底线：
#   * 绝不在新版构建成功前动正在跑的二进制；
#   * 迁移用新版二进制跑，跑完才换版；迁移失败时旧版毫发无损；
#   * 迁移一旦跑完不自动回滚（goose 迁移默认只向前，如需回退请人工处理）；
#   * Web 前端产物（web/portal、web/workbench）不在本脚本范围，
#     检测到 web 源码变化时只打警告，需另行构建部署。
#
# 用法：
#   morde-update.sh [--branch main] [--dry-run] [--skip-migrate]
#                   [--keep-releases N] [--keep-backups N] [--force]
#   --dry-run: 只做到构建+校验+迁移状态检查，不备份、不换版、
#              不跑迁移、不重启。用于演练和验证。
#   --skip-migrate: 跳过 `portal migrate up`（迁移由人工另跑）。
#   --force: 即使已是最新版也强制重建重装。
#
# 需要用户定的事（默认值在下面 DEFAULTS 段）：
#   BRANCH（默认 main）、是否自动跑迁移（默认跑）、保留版本数（默认各 3 份）。

set -euo pipefail

# ---------------- defaults（可由环境变量或命令行覆盖）----------------
REPO_URL="${REPO_URL:-https://github.com/lizhongxuan/morde.git}"
BRANCH="${BRANCH:-main}"
SRC_DIR="${SRC_DIR:-/opt/morde-src}"
MORDE_DIR="${MORDE_DIR:-/opt/morde}"
BIN="${BIN:-/opt/morde/bin/portal}"
CONFIG="${CONFIG:-/opt/morde/deploy/single/portal.yaml}"
ENV_FILE="${ENV_FILE:-/etc/cloudhost/cloudhost.env}"
SERVICE="${SERVICE:-morde-portal.service}"
PORTAL_USER="${PORTAL_USER:-morde}"
GO_VERSION="${GO_VERSION:-1.27.1}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
KEEP_RELEASES="${KEEP_RELEASES:-3}"
KEEP_BACKUPS="${KEEP_BACKUPS:-3}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:7080/healthz}"
READY_URL="${READY_URL:-http://127.0.0.1:7080/readyz}"
HEALTH_WAIT="${HEALTH_WAIT:-90}"
LOG_FILE="${LOG_FILE:-/var/log/morde-update.log}"
LOCK_FILE="${LOCK_FILE:-/var/lock/morde-update.lock}"
DRY_RUN=0
SKIP_MIGRATE=0
FORCE=0

# ---------------- state ----------------
SWAPPED=0
CONFIG_SYNCED=0
BACKUP_DIR=""
NEW_BIN=""
NEW_HEAD=""
RELEASE_DIR=""
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"

# Go may already be installed at /usr/local/go (by a previous run) without
# being on PATH — make it visible before any `command -v go` check.
export PATH="/usr/local/go/bin:$PATH"

# ---------------- helpers ----------------
log() {
  local msg="[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"
  echo "$msg"
  echo "$msg" >>"$LOG_FILE" 2>/dev/null || true
}

die() {
  log "FATAL: $*"
  if [[ "$SWAPPED" == "1" ]]; then
    on_error 1
  fi
  exit 1
}

usage() {
  sed -n '2,30p' "$0"
  exit "${1:-0}"
}

# 以 morde 用户、带 EnvironmentFile 跑一次 portal 子命令（只读/迁移都用它，
# 绝不在 bash 里 source cloudhost.env——里面有多行 Caddy 配置）。
# 用法：portal_once <binary> <args...>
portal_once() {
  local bin="$1"
  shift
  systemd-run --wait --collect --pipe \
    -p "User=$PORTAL_USER" \
    -p "WorkingDirectory=$MORDE_DIR" \
    -p "EnvironmentFile=$ENV_FILE" \
    "$bin" "$@" --config "$CONFIG"
}

# 跑一次 `portal migrate up|status`，把输出逐行记进日志；
# 失败时把完整输出也记进日志再返回非零（调用处触发 ERR trap 中止更新）。
# 用法：run_migrate <binary> <up|status>
run_migrate() {
  local bin="$1" action="$2" out
  out="$(mktemp)"
  if portal_once "$bin" migrate "$action" >"$out" 2>&1; then
    while IFS= read -r line; do log "  migrate-$action: $line"; done <"$out"
    rm -f "$out"
    return 0
  fi
  log "migrate $action FAILED (binary: $bin); full output:"
  while IFS= read -r line; do log "  migrate-$action: $line"; done <"$out"
  rm -f "$out"
  return 1
}

wait_healthy() {
  local deadline=$((SECONDS + HEALTH_WAIT))
  while ((SECONDS < deadline)); do
    if systemctl is-active --quiet "$SERVICE" \
      && [[ "$(curl -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null || true)" == "ok" ]] \
      && curl -fsS --max-time 5 -o /dev/null "$READY_URL" 2>/dev/null; then
      return 0
    fi
    sleep 3
  done
  return 1
}

restore_model_config() {
  [[ -n "$BACKUP_DIR" && -f "$BACKUP_DIR/portal.yaml" ]] || return 1
  local restore_tmp
  restore_tmp="$(mktemp "$(dirname "$CONFIG")/.$(basename "$CONFIG").restore.XXXXXX")" || return 1
  if ! cp -a "$BACKUP_DIR/portal.yaml" "$restore_tmp" || ! mv -f "$restore_tmp" "$CONFIG"; then
    rm -f "$restore_tmp"
    return 1
  fi
  CONFIG_SYNCED=0
  log "restored portal config from $BACKUP_DIR/portal.yaml"
}

rollback() {
  trap - ERR
  log "ERROR: update failed after swap, rolling back to backup binary"
  if [[ "$CONFIG_SYNCED" == "1" ]]; then
    restore_model_config || log "ROLLBACK WARNING: could not restore portal config; backup: $BACKUP_DIR/portal.yaml"
  fi
  if [[ -n "$BACKUP_DIR" && -f "$BACKUP_DIR/portal" ]]; then
    install -m 0755 "$BACKUP_DIR/portal" "$BIN.new"
    mv -f "$BIN.new" "$BIN"
    systemctl restart "$SERVICE" || true
    if wait_healthy; then
      log "ROLLBACK OK: old binary is serving again"
      log "NOTE: migrations already applied were NOT rolled back (goose is forward-only)"
    else
      log "ROLLBACK FAILED: manual intervention required. Backup binary: $BACKUP_DIR/portal"
    fi
  else
    log "ROLLBACK IMPOSSIBLE: no backup binary recorded"
  fi
}

on_error() {
  local rc="$1"
  trap - ERR
  if [[ "$SWAPPED" == "1" ]]; then
    rollback
  else
    if [[ "$CONFIG_SYNCED" == "1" ]]; then
      restore_model_config || log "ERROR: could not restore portal config; backup: $BACKUP_DIR/portal.yaml"
    fi
    log "ERROR: update aborted before swap (rc=$rc); running binary untouched"
  fi
  exit "$rc"
}

trap 'on_error $?' ERR

prune_old() {
  # $1=目录 $2=glob 模式 $3=保留份数 $4=绝不能删的那个（可为空）
  local dir="$1" pattern="$2" keep="$3" keep_this="${4:-}"
  local -a entries
  mapfile -t entries < <(ls -1d "$dir"/$pattern 2>/dev/null | sort || true)
  local total="${#entries[@]}"
  if ((total <= keep)); then
    return 0
  fi
  local drop=$((total - keep))
  local i d
  for ((i = 0; i < drop; i++)); do
    d="${entries[$i]}"
    if [[ -n "$keep_this" && "$d" == "$keep_this" ]]; then
      continue
    fi
    log "prune: removing old $d"
    rm -rf -- "$d"
  done
}

# ---------------- args ----------------
while (($# > 0)); do
  case "$1" in
    --branch)
      BRANCH="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --skip-migrate)
      SKIP_MIGRATE=1
      shift
      ;;
    --keep-releases)
      KEEP_RELEASES="$2"
      shift 2
      ;;
    --keep-backups)
      KEEP_BACKUPS="$2"
      shift 2
      ;;
    --force)
      FORCE=1
      shift
      ;;
    -h | --help)
      usage 0
      ;;
    *)
      die "unknown argument: $1 (see --help)"
      ;;
  esac
done

# ---------------- preflight ----------------
[[ "$(id -u)" == "0" ]] || die "must run as root"
exec 9>"$LOCK_FILE" || die "cannot open lock file $LOCK_FILE"
flock -n 9 || die "another morde-update is running (lock: $LOCK_FILE)"
log "=== morde-update start: branch=$BRANCH dry-run=$DRY_RUN skip-migrate=$SKIP_MIGRATE ==="

for cmd in git curl systemctl python3 flock sha256sum; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
done
systemctl is-active --quiet docker.service || die "docker.service is not active"
docker ps --filter "name=^cloudhost-postgres$" --format '{{.Status}}' 2>/dev/null | grep -qi '^up' \
  || die "cloudhost-postgres container is not up"
avail_kb="$(df --output=avail -k /opt | tail -1 | tr -d ' ')"
((avail_kb >= 3000000)) || die "not enough free disk on /opt: ${avail_kb}KB"
[[ -f "$ENV_FILE" ]] || die "missing env file: $ENV_FILE"
[[ -f "$CONFIG" ]] || die "missing portal config: $CONFIG"
[[ -x "$BIN" ]] || die "missing running binary: $BIN"

# --- E10: environment-down detection ---
# The DSH daemon (cloud-computer tasks cannot spawn without it) and the
# workspace-sync mount (the portal requires it) are checked here, before
# any deploy work. A failure stops the deploy with the cause named,
# instead of deploying a release that silently cannot run tasks.
# This is the deploy-level exception gate: when the environment is down
# the deploy stops and says why, instead of blaming the code in the
# smoke record. The smoke gate below has its own gate-level exception
# (cases the environment cannot run, e.g. T10/T43 needing a real PC);
# those are notices, not failures, and the deploy proceeds with the
# reduced evidence on record.
# R20's full runtime interruption-delivery chain is a separate,
# contract-blocked piece and is not part of this preflight.
dsh_enabled="$(sed -n '/^cloud_dsh:/,/^[^[:space:]]/p' "$CONFIG" | awk '/^[[:space:]]*enabled:/ {print $2; exit}')"
dsh_host="$(sed -n '/^cloud_dsh:/,/^[^[:space:]]/p' "$CONFIG" | awk '/^[[:space:]]*docker_host:/ {print $2; exit}')"
dsh_host="${dsh_host:-unix:///run/morde-dsh-docker/docker.sock}"
if [[ "${dsh_enabled:-true}" != "false" ]]; then
  dsh_sock="${dsh_host#unix://}"
  if [[ ! -S "$dsh_sock" ]]; then
    die "cloud DSH daemon is not reachable: socket $dsh_sock is missing (docker_host=$dsh_host). Cloud-computer tasks cannot spawn after a deploy; check the DSH daemon (systemctl status morde-dsh-docker) and retry. Deploy aborted."
  fi
  if ! docker -H "$dsh_host" info >/dev/null 2>&1; then
    die "cloud DSH daemon at $dsh_host does not answer docker info. Cloud-computer tasks cannot spawn after a deploy; check the DSH daemon and retry. Deploy aborted."
  fi
fi
if ! grep -q " /srv/morde-acceptance " /proc/mounts; then
  die "workspace-sync mount /srv/morde-acceptance is not mounted (the portal requires it). Check /etc/fstab and run: mount /srv/morde-acceptance. Deploy aborted."
fi

CURRENT_SHA="$(sha256sum "$BIN" | awk '{print $1}')"
log "running binary: $BIN sha256=$CURRENT_SHA"
if systemctl is-active --quiet "$SERVICE"; then
  log "service $SERVICE is active"
else
  die "service $SERVICE is not active; refusing to update a stopped service"
fi

log "baseline migrate status (current binary, read-only):"
run_migrate "$BIN" status

# ---------------- ensure Go ----------------
ensure_go() {
  local ver=""
  if command -v go >/dev/null 2>&1; then
    ver="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
    if [[ -n "$ver" ]] && printf '1.25.0\n%s\n' "$ver" | sort -V -C 2>/dev/null; then
      log "Go already present: $(go version)"
      return 0
    fi
    log "Go $ver is too old (< 1.25.0), installing Go $GO_VERSION"
  else
    log "Go not found, installing Go $GO_VERSION"
  fi
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' RETURN
  curl -fsSL -o "$tmpdir/go.tgz" "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz"
  curl -fsSL -o "$tmpdir/go.tgz.sha256" "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz.sha256"
  local want got
  want="$(awk '{print $1}' "$tmpdir/go.tgz.sha256")"
  got="$(sha256sum "$tmpdir/go.tgz" | awk '{print $1}')"
  [[ "$want" == "$got" ]] || die "Go tarball sha256 mismatch (want $want, got $got)"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmpdir/go.tgz"
  export PATH="/usr/local/go/bin:$PATH"
  log "Go installed: $(go version)"
}
ensure_go
export GOPROXY
export CGO_ENABLED=0

# ---------------- checkout ----------------
if [[ ! -d "$SRC_DIR/.git" ]]; then
  log "cloning $REPO_URL (branch $BRANCH) into $SRC_DIR"
  git clone --branch "$BRANCH" "$REPO_URL" "$SRC_DIR"
  OLD_SRC_HEAD=""
else
  OLD_SRC_HEAD="$(git -C "$SRC_DIR" rev-parse HEAD)"
  log "fetching $REPO_URL"
  git -C "$SRC_DIR" fetch --prune origin
  git -C "$SRC_DIR" checkout "$BRANCH"
  git -C "$SRC_DIR" reset --hard "origin/$BRANCH"
fi
NEW_HEAD="$(git -C "$SRC_DIR" rev-parse HEAD)"
log "source: ${OLD_SRC_HEAD:-<fresh clone>} -> $NEW_HEAD"
git -C "$SRC_DIR" log --oneline -5 | while IFS= read -r line; do log "  commit: $line"; done

MODEL_ALLOWLIST_SYNC="$SRC_DIR/deploy/single/sync-model-allowlist.py"
[[ -f "$MODEL_ALLOWLIST_SYNC" ]] || die "missing model allowlist sync helper: $MODEL_ALLOWLIST_SYNC"
CONFIG_SYNC_NEEDED=0
if python3 "$MODEL_ALLOWLIST_SYNC" --check "$SRC_DIR/deploy/single/portal.yaml" "$CONFIG"; then
  log "active model host allowlist matches repository defaults"
else
  check_rc=$?
  [[ "$check_rc" == "1" ]] || die "could not validate active model allowlist (helper exit $check_rc)"
  CONFIG_SYNC_NEEDED=1
  log "active model host allowlist is missing repository defaults; it will be updated while deploying"
fi

# idempotency: already on this HEAD and binary matches the recorded deploy
if [[ "$FORCE" == "0" && -f "$MORDE_DIR/.deployed-head" ]]; then
  read -r deployed_head deployed_sha <"$MORDE_DIR/.deployed-head" || true
  if [[ "$deployed_head" == "$NEW_HEAD" && "$deployed_sha" == "$CURRENT_SHA" && "$CONFIG_SYNC_NEEDED" == "0" ]]; then
    log "already up to date at $NEW_HEAD (use --force to rebuild anyway)"
    exit 0
  fi
fi

# web 源码变化只警告，不由本脚本处理
if [[ -n "${OLD_SRC_HEAD:-}" ]]; then
  if git -C "$SRC_DIR" diff --name-only "$OLD_SRC_HEAD" "$NEW_HEAD" -- web/portal web/workbench | grep -q .; then
    log "WARNING: web/portal or web/workbench changed in this update; web assets are NOT rebuilt by this script — rebuild and redeploy them separately"
  fi
fi

# ---------------- P0 smoke gate (E10) ----------------
# No deploy without a passing smoke record. The gate runs the ten P0
# smoke cases (T01/T04/T06/T10/T13/T19/T22/T26/T36/T43) against the new
# head; any failure aborts before the build, so a broken head never
# reaches the swap. The gate is fail-closed: a case that produces no
# evidence fails, unless the environment itself cannot run it — then
# it is recorded as an exception-gate notice, not a failure (T10/T43
# need a real PC; PostgreSQL cases without CH_TEST_DATABASE_URL).
# Environment-down (dead DSH daemon, missing workspace-sync mount)
# was already caught by the preflight above, which aborts with the
# cause named before this gate runs. The record is the deploy evidence.
SMOKE_RECORD="$MORDE_DIR/smoke-record.json"
log "running P0 smoke gate for $NEW_HEAD (record: $SMOKE_RECORD)"
if ! bash "$SRC_DIR/deploy/single/smoke.sh" --src "$SRC_DIR" --record "$SMOKE_RECORD"; then
  die "P0 smoke gate failed for $NEW_HEAD — see $SMOKE_RECORD; deploy aborted, current release untouched"
fi

# ---------------- build ----------------
RELEASE_NAME="${TIMESTAMP}-${NEW_HEAD:0:8}"
RELEASE_DIR="$MORDE_DIR/releases/$RELEASE_NAME"
mkdir -p "$RELEASE_DIR/.local/bin"
NEW_BIN="$RELEASE_DIR/.local/bin/portal"
log "building portal ($NEW_HEAD) into $NEW_BIN"
cd "$SRC_DIR/server"
GOOS=linux GOARCH=amd64 go build -o "$NEW_BIN" ./cmd/portal
cd "$MORDE_DIR"
[[ -x "$NEW_BIN" ]] || die "build produced no binary at $NEW_BIN"

# ---------------- validate new binary ----------------
log "validating new binary"
file "$NEW_BIN"
python3 "$SRC_DIR/deploy/single/validate-portal-binary.py" "$NEW_BIN" "$(uname -m)" \
  || die "binary failed arch validation"
go version -m "$NEW_BIN" | head -5 | while IFS= read -r line; do log "  go-version-m: $line"; done
NEW_SHA="$(sha256sum "$NEW_BIN" | awk '{print $1}')"
log "new binary sha256=$NEW_SHA (running: $CURRENT_SHA)"
[[ "$NEW_SHA" != "$CURRENT_SHA" ]] || log "NOTE: new binary is byte-identical to the running one"

log "migrate status with NEW binary (read-only):"
run_migrate "$NEW_BIN" status

if [[ "$DRY_RUN" == "1" ]]; then
  log "=== DRY RUN OK: checkout + build + validation + migrate status all passed ==="
  if [[ "$CONFIG_SYNC_NEEDED" == "1" ]]; then
    log "=== DRY RUN: active model host allowlist would be updated; no config was changed ==="
  fi
  log "=== DRY RUN: stopping here — no backup, no swap, no migrate up, no restart ==="
  log "=== DRY RUN: built binary kept at $NEW_BIN (release dir will be pruned on the next real run) ==="
  exit 0
fi

# ---------------- backup current version ----------------
BACKUP_DIR="$MORDE_DIR/backups/pre-update-$TIMESTAMP"
install -d -m 0700 "$BACKUP_DIR"
cp -a "$BIN" "$BACKUP_DIR/portal"
{
  echo "timestamp=$TIMESTAMP"
  echo "branch=$BRANCH"
  echo "old_src_head=${OLD_SRC_HEAD:-<fresh clone>}"
  echo "new_src_head=$NEW_HEAD"
  echo "old_binary_sha256=$CURRENT_SHA"
  echo "new_binary_sha256=$NEW_SHA"
  echo "release_dir=$RELEASE_DIR"
} >"$BACKUP_DIR/MANIFEST"
log "backed up running binary to $BACKUP_DIR/portal"

# ---------------- migrate up (new binary, before restart) ----------------
if [[ "$SKIP_MIGRATE" == "1" ]]; then
  log "SKIP_MIGRATE=1: NOT running migrate up — run it manually before the new binary can serve"
else
  log "running migrate up with NEW binary"
  run_migrate "$NEW_BIN" up
  log "migrate status after up:"
  run_migrate "$NEW_BIN" status
fi

# Keep operator-added hosts and all unrelated runtime settings; only merge
# repository-declared model host defaults into the active config.
if [[ "$CONFIG_SYNC_NEEDED" == "1" ]]; then
  cp -a "$CONFIG" "$BACKUP_DIR/portal.yaml"
  echo "active_config_backup=portal.yaml" >>"$BACKUP_DIR/MANIFEST"
  log "backed up active portal config to $BACKUP_DIR/portal.yaml"
  CONFIG_SYNCED=1
  python3 "$MODEL_ALLOWLIST_SYNC" --sync "$SRC_DIR/deploy/single/portal.yaml" "$CONFIG"
fi

# ---------------- swap + restart ----------------
log "swapping in new binary (atomic rename)"
install -m 0755 "$NEW_BIN" "$BIN.new"
mv -f "$BIN.new" "$BIN"
SWAPPED=1
log "restarting $SERVICE (daemon-reload first: unit file changed on disk)"
systemctl daemon-reload
systemctl restart "$SERVICE"

if wait_healthy; then
  log "health check OK: $HEALTH_URL = ok, $READY_URL = 200"
else
  die "health check FAILED after restart"
fi

# verify listeners are back
for port in 7080 7081 7082; do
  ss -tln 2>/dev/null | grep -q "127.0.0.1:$port" || die "port $port is not listening after restart"
done
log "listeners 7080/7081/7082 are up"

# ---------------- record + prune ----------------
printf '%s\n%s\n' "$NEW_HEAD" "$NEW_SHA" >"$MORDE_DIR/.deployed-head"
prune_old "$MORDE_DIR/releases" '[0-9]*T[0-9]*Z-*' "$KEEP_RELEASES" "$RELEASE_DIR"
prune_old "$MORDE_DIR/backups" 'pre-update-*' "$KEEP_BACKUPS" "$BACKUP_DIR"
log "=== morde-update OK: $OLD_SRC_HEAD -> $NEW_HEAD (release $RELEASE_NAME) ==="
log "=== backup kept at $BACKUP_DIR (rollback: copy portal back to $BIN and restart) ==="
