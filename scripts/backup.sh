#!/usr/bin/env bash
# =============================================================================
#  云枢 API 中转站 · 全量逻辑备份
# =============================================================================
#  作用：用容器内的 mysqldump 导出 yunshu（主库）与 yunshu_log（日志库），
#        每个库一个 gzip 文件，外加一份同名 .manifest 记录校验与对账基线：
#          backups/yunshu-YYYYmmdd-HHMMSS.sql.gz      主库 dump
#          backups/yunshu_log-YYYYmmdd-HHMMSS.sql.gz  日志库 dump
#          backups/yunshu-YYYYmmdd-HHMMSS.manifest    清单（库名 / MAX(version) /
#                                                     表数 / 6 张关键表行数 / sha256）
#
#  口令：从 deploy/.env.stack 的 MYSQL_ROOT_PASSWORD 读取，经 MYSQL_PWD 环境
#        变量传入容器。注意这里是 `MYSQL_PWD="$pw" docker exec -e MYSQL_PWD`：
#        -e 不带值表示从 docker 客户端的环境继承，因此口令既不在容器内
#        mysql/mysqldump 的 argv 里，也不在宿主 docker.exe 的 argv 里。
#
#  为什么不用 mysqldump --databases：那会写入 CREATE DATABASE / USE，恢复时
#        无法指定目标库（导入 yunshu_drill 会被改写成 yunshu）。不带它导出的
#        是一串 DROP TABLE IF EXISTS + CREATE TABLE，恢复到任意库都安全。
#
#  一致性口径：--single-transaction 在导出开始时取一个快照；manifest 里的
#        行数是导出前另一条连接查的，线上有写入时可能相差几行。演练应在低峰
#        或停写窗口跑，行数比对以 manifest 为准（段 2 报告里记录偏差）。
#
#  用法（仓库根目录）：
#      bash scripts/backup.sh                   # 备份 yunshu + yunshu_log，保留 7 份
#      bash scripts/backup.sh --keep 3          # 只保留最近 3 份
#      bash scripts/backup.sh --dbs "yunshu"    # 只备份主库
#      bash scripts/backup.sh --dir /data/bak   # 换存放目录
#
#  退出码：0 成功；1 前置失败或导出失败（不会留下半截 .sql.gz / .manifest）。
# =============================================================================

set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
ENV_FILE="${YUNSHU_ENV_FILE:-$ROOT_DIR/deploy/.env.stack}"

CONTAINER="${YUNSHU_MYSQL_CONTAINER:-yunshu-stack-mysql}"
BACKUP_DIR="${YUNSHU_BACKUP_DIR:-$ROOT_DIR/backups}"
DBS="${YUNSHU_BACKUP_DBS:-yunshu yunshu_log}"
KEEP="${YUNSHU_BACKUP_KEEP:-7}"

# 恢复脚本与演练报告比对的就是这 6 张表：账户、批次、流水、用量、充值单。
KEY_TABLES="t_user t_credit_account t_credit_batch t_ledger_entry t_usage_record t_recharge_order"

MYSQL_ROOT_PASSWORD_VALUE=""
TS=""
WORK_FILES=()

if [ -t 1 ]; then
  C_INFO=$'\033[36m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_OFF=$'\033[0m'
else
  C_INFO=''; C_WARN=''; C_ERR=''; C_OFF=''
fi

info() { printf '%s[backup]%s %s\n' "$C_INFO" "$C_OFF" "$*"; }
warn() { printf '%s[backup] WARN%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die()  { printf '%s[backup] FATAL%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2; exit 1; }

# 备份里是用户行与密钥哈希，权限按 0600 / 0700 落盘。
umask 077

cleanup() {
  local f
  if [ "${#WORK_FILES[@]}" -gt 0 ]; then
    for f in "${WORK_FILES[@]}"; do
      rm -f -- "$f" 2>/dev/null || true
    done
  fi
}
trap cleanup EXIT

usage() {
  awk 'NR > 1 { if ($0 !~ /^#/) exit; sub(/^# ?/, ""); print }' "$0"
}

# ---------------------------------------------------------------------------
# 环境与 MySQL 访问
# ---------------------------------------------------------------------------

# env_get KEY：从 .env.stack 取值，去掉行尾注释与包裹引号。
env_get() {
  local key="$1"
  [ -f "$ENV_FILE" ] || return 0
  awk -v k="$key" '
    index($0, k "=") == 1 {
      v = substr($0, length(k) + 2)
      sub(/[ \t]+#.*$/, "", v)
      gsub(/^[ \t]+|[ \t]+$/, "", v)
      if (v ~ /^".*"$/ || v ~ /^\047.*\047$/) {
        v = substr(v, 2, length(v) - 2)
      }
      found = v
    }
    END { print found }
  ' "$ENV_FILE"
}

# mysql_q SQL：-N -B 输出裸值，供脚本解析。
mysql_q() {
  MYSQL_PWD="$MYSQL_ROOT_PASSWORD_VALUE" docker exec -i -e MYSQL_PWD "$CONTAINER" \
    mysql --default-character-set=utf8mb4 -uroot -N -B -e "$1"
}

db_exists() {
  [ "$(mysql_q "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = '$1'")" = "1" ]
}

db_version() {
  local db="$1" has
  has=$(mysql_q "SELECT COUNT(*) FROM information_schema.tables
                  WHERE table_schema = '$db' AND table_name = 'schema_migrations'")
  if [ "$has" = "1" ]; then
    mysql_q "SELECT COALESCE(MAX(version), '') FROM \`$db\`.schema_migrations"
  fi
  return 0
}

db_table_count() {
  mysql_q "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '$1'"
}

table_exists() {
  [ "$(mysql_q "SELECT COUNT(*) FROM information_schema.tables
                 WHERE table_schema = '$1' AND table_name = '$2'")" = "1" ]
}

# human_size BYTES：只用于终端展示，manifest 里存精确字节数。
human_size() {
  local b="$1"
  if [ "$b" -ge 1073741824 ] 2>/dev/null; then
    awk -v b="$b" 'BEGIN { printf "%.2f GiB", b / 1073741824 }'
  elif [ "$b" -ge 1048576 ] 2>/dev/null; then
    awk -v b="$b" 'BEGIN { printf "%.2f MiB", b / 1048576 }'
  elif [ "$b" -ge 1024 ] 2>/dev/null; then
    awk -v b="$b" 'BEGIN { printf "%.2f KiB", b / 1024 }'
  else
    printf '%s B' "$b"
  fi
}

# ---------------------------------------------------------------------------
# 参数与前置检查
# ---------------------------------------------------------------------------

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --keep)
        [ $# -ge 2 ] || die "--keep 需要值"
        KEEP="$2"; shift 2 ;;
      --dbs)
        [ $# -ge 2 ] || die "--dbs 需要值"
        DBS="$2"; shift 2 ;;
      --dir)
        [ $# -ge 2 ] || die "--dir 需要值"
        BACKUP_DIR="$2"; shift 2 ;;
      --help|-h)
        usage; exit 0 ;;
      *)
        die "未知参数：$1（只支持 --keep / --dbs / --dir / --help）" ;;
    esac
  done

  case "$KEEP" in
    ''|*[!0-9]*) die "--keep 必须是正整数：'$KEEP'" ;;
  esac
  [ "$KEEP" -ge 1 ] || die "--keep 必须 ≥ 1"

  local db
  for db in $DBS; do
    case "$db" in
      ''|*[!A-Za-z0-9_]*) die "数据库名非法：'$db'" ;;
      yunshu_test|yunshu_recon_test|*_test)
        die "拒绝备份测试库 $db：T36 硬约束 2（yunshu_test 不属备份范围）" ;;
      mysql|sys|performance_schema|information_schema)
        die "拒绝备份系统库 $db" ;;
    esac
  done
}

preflight() {
  [ -f "$ENV_FILE" ] || die "找不到环境文件：$ENV_FILE（先 cp deploy/.env.stack.example deploy/.env.stack）"
  command -v docker >/dev/null 2>&1 || die "PATH 里没有 docker"
  command -v gzip >/dev/null 2>&1 || die "PATH 里没有 gzip"
  command -v sha256sum >/dev/null 2>&1 || die "PATH 里没有 sha256sum"

  MYSQL_ROOT_PASSWORD_VALUE=$(env_get MYSQL_ROOT_PASSWORD)
  [ -n "$MYSQL_ROOT_PASSWORD_VALUE" ] || die "$ENV_FILE 里没有 MYSQL_ROOT_PASSWORD"

  local running
  running=$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || true)
  [ "$running" = "true" ] || die "容器 $CONTAINER 不存在或未运行（先跑 scripts/stack.sh up）"

  [ "$(mysql_q 'SELECT 1')" = "1" ] || die "无法连接 $CONTAINER 上的 MySQL"

  local db
  for db in $DBS; do
    db_exists "$db" || die "库 $db 不存在（检查 --dbs；可选：yunshu / yunshu_log）"
  done
}

# ---------------------------------------------------------------------------
# 导出
# ---------------------------------------------------------------------------

dump_db() {
  local db="$1" out="$2"
  MYSQL_PWD="$MYSQL_ROOT_PASSWORD_VALUE" docker exec -e MYSQL_PWD "$CONTAINER" \
    mysqldump \
      --default-character-set=utf8mb4 \
      --single-transaction \
      --quick \
      --routines \
      --triggers \
      --set-gtid-purged=OFF \
      --no-tablespaces \
      --hex-blob \
      "$db" \
    | gzip -c >"$out"
}

# collect_stats DB OUTFILE：导出前取版本 / 表数 / 关键表行数。
# 放在 dump 之前是口径选择：导出快照晚于计数，恢复后的行数只会 ≥ 这里记录
# 的值（期间有写入）。反过来先导出再计数，删除一行就会让恢复被误判成丢数据。
collect_stats() {
  local db="$1" out="$2" t n
  {
    printf 'schema_version=%s\n' "$(db_version "$db")"
    printf 'table_count=%s\n' "$(db_table_count "$db")"
    for t in $KEY_TABLES; do
      if table_exists "$db" "$t"; then
        n=$(mysql_q "SELECT COUNT(*) FROM \`$db\`.\`$t\`")
        printf 'rows.%s=%s\n' "$t" "$n"
      fi
    done
  } >"$out"
}

# write_db_block DB FILE STATSFILE OUT：把文件校验值与预采统计写入 manifest。
write_db_block() {
  local db="$1" file="$2" stats="$3" out="$4"
  local bytes sha

  bytes=$(wc -c <"$file" | tr -d ' ')
  sha=$(sha256sum "$file" | cut -d' ' -f1)

  {
    printf '[db %s]\n' "$db"
    printf 'file=%s\n' "$(basename "$file")"
    printf 'bytes=%s\n' "$bytes"
    printf 'sha256=%s\n' "$sha"
    cat "$stats"
    printf '\n'
  } >>"$out"
}

# prune_old：按 manifest 文件名倒序保留最近 KEEP 份，连同 dump 一起删。
prune_old() {
  local n=0 m f base
  while IFS= read -r m; do
    n=$((n + 1))
    [ "$n" -le "$KEEP" ] && continue
    base=$(basename "$m" .manifest)
    while IFS= read -r f; do
      f=${f#file=}
      case "$f" in
        ''|*/*) continue ;;
      esac
      rm -f -- "$BACKUP_DIR/$f"
    done < <(grep '^file=' "$m" || true)
    rm -f -- "$m"
    info "  清理旧备份集 $base"
  done < <(ls -1 "$BACKUP_DIR"/*.manifest 2>/dev/null | sort -r)
  return 0
}

# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------

main() {
  parse_args "$@"
  preflight

  mkdir -p -- "$BACKUP_DIR"

  TS=$(date -u +%Y%m%d-%H%M%S)
  local primary_db manifest dump_size=0 db file tmp stats
  local empty_dbs=()
  primary_db=${DBS%% *}
  manifest="$BACKUP_DIR/$primary_db-$TS.manifest"

  local man_tmp="$BACKUP_DIR/.$primary_db-$TS.manifest.partial"
  WORK_FILES+=("$man_tmp")
  : >"$man_tmp"

  info "开始备份：库 [$DBS]，容器 $CONTAINER，时间戳 $TS（UTC）"

  {
    printf '# yunshu backup manifest — 由 scripts/backup.sh 生成，scripts/restore.sh 校验并读取\n'
    printf 'backup_id=%s-%s\n' "$primary_db" "$TS"
    printf 'created_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'created_at_local=%s\n' "$(date +%Y-%m-%dT%H:%M:%S%z)"
    printf 'git_commit=%s\n' "$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
    printf 'container=%s\n' "$CONTAINER"
    printf 'mysql_server=%s\n' "$(mysql_q 'SELECT VERSION()')"
    printf 'dump_tool=mysqldump --single-transaction --routines --triggers --set-gtid-purged=OFF\n'
    printf 'dbs=%s\n' "$DBS"
    printf 'key_tables=%s\n\n' "$KEY_TABLES"
  } >>"$man_tmp"

  # 先取值再导出：见 collect_stats 的口径说明。
  for db in $DBS; do
    stats="$BACKUP_DIR/.$db-$TS.stats.partial"
    WORK_FILES+=("$stats")
    collect_stats "$db" "$stats"
  done

  for db in $DBS; do
    file="$BACKUP_DIR/$db-$TS.sql.gz"
    tmp="$file.partial"
    WORK_FILES+=("$tmp")

    info "  导出 $db → $(basename "$file")"
    if ! dump_db "$db" "$tmp"; then
      die "mysqldump 失败：$db（未生成任何 .sql.gz）"
    fi
    [ -s "$tmp" ] || die "导出 $db 得到空文件（检查 mysqldump 权限与库是否存在）"
    gzip -t "$tmp" || die "gzip 校验失败：$db"

    mv -f -- "$tmp" "$file"
    write_db_block "$db" "$file" "$BACKUP_DIR/.$db-$TS.stats.partial" "$man_tmp"
    dump_size=$((dump_size + $(wc -c <"$file" | tr -d ' ')))

    # 0 张表的库是一次"成功但零内容"的备份：mysqldump 退出码 0、gzip 校验通过、
    # manifest 写得整整齐齐，演练报告据此就会判通过——而真正需要恢复时这个库是
    # 空的。2026-11-06 的演练里 yunshu_log 正是这个形态（471 字节空 dump）。
    # 记为 WARN 而不是 FATAL：库可能确实是新建的或本就不该有表，让脚本因此
    # 中断会把正常备份也一起挡掉。判定口径放在这里、不写进 manifest，是为了不
    # 改动 restore.sh 已经在读的清单格式。
    if [ "$(sed -n 's/^table_count=//p' "$BACKUP_DIR/.$db-$TS.stats.partial")" = "0" ]; then
      empty_dbs+=("$db")
    fi
  done

  mv -f -- "$man_tmp" "$manifest"

  info "备份完成：$(basename "$manifest")（总计 $(human_size "$dump_size")）"
  grep -E '^(file|bytes|sha256|schema_version|table_count)=' "$manifest" | sed 's/^/    /'

  if [ "${#empty_dbs[@]}" -gt 0 ]; then
    warn "以下库导出成功但 table_count=0，恢复后是空库：${empty_dbs[*]}"
    warn "  这是「成功但零内容」的备份，演练报告不得据此判定该库可恢复；"
    warn "  若该库本该有表（例如日志库），说明部署未执行对应迁移——先去确认，不要当通过。"
  fi

  prune_old

  info "保留策略：最近 $KEEP 份 manifest 及对应 dump"
  info "产物目录：$BACKUP_DIR"
}

main "$@"
