#!/usr/bin/env bash
# check-model-output-ceilings.sh — t_model 输出上限越界巡检（T48）。
#
# 查什么：t_model 里「输出上限高于自己的上下文长度」的行，即
#
#   SELECT id, model_name, context_length, max_output_tokens
#     FROM t_model
#    WHERE deleted_at IS NULL
#      AND context_length > 0
#      AND max_output_tokens > context_length;
#
# 为什么这是一条要巡检的脏状态：context_length 是输入＋输出。上限越过它，
# 说明这一行的两个字段互相矛盾。网关估算（internal/gw/p09_estimate.go）在
# max_output_tokens > 0 时直接拿它当输出上限 return，不会再裁剪到
# context_length，于是 prehold 会按一个模型根本产不出的输出量预留额度。预扣是
# 上限、结算按真实 usage，所以不产生资损；代价是余额偏紧的用户可能被 402
# 误拦，同时上游仍会按真实上下文拒请求。
#
# 本卡的 PATCH 校验（internal/control/ops/model.go, context_length 分支）只
# 拦「新写入」的越界。存量行按 T46 §6 的语义不回填（max_output_tokens = 0 就是
# 「未配置」），因此需要这支脚本把它们找出来，由运营用一次 PATCH 把两个字段
# 一起改对。
#
# 只诊断，不订正：脚本内只有上面那一条 SELECT，没有 --fix、没有 --sql 入口，
# 也不调用任何写接口。跑多少次、跑在哪个环境都不会改一行数据。
#
# 退出码：
#   0  没有越界行（输出「共 0 行越界」）
#   1  有越界行（打印明细 + 行数）——可直接挂 CI 或人工巡检
#   2  前置条件不满足（解析不出 DSN / 没有可用的 mysql 客户端 / 查询失败）
#      ——「查不了」不能冒充「没问题」，所以绝不返回 0
#
# 连接来源（优先级从高到低）：
#   1. 环境变量 DB_DSN（格式同 .env，例如
#      'root:root@tcp(127.0.0.1:13306)/yunshu?parseTime=true'）
#   2. DB_DSN 未设置时，从 $YUNSHU_ENV_FILE 指定的文件里读，否则依次试
#      deploy/.env.stack、.env
#   3. MYSQL_HOST / MYSQL_PORT / MYSQL_USER / MYSQL_PASSWORD / YUNSHU_DB_NAME
#      逐项覆盖解析结果（用于口令不写在 DSN 里的环境）
#
# 为什么不 source 那个 env 文件：它是数据不是脚本，`set -a; . file` 会把文件里
# 任何一行都当命令执行（含空格的变量值会直接变成一次命令调用），而本脚本只是
# 想读一个 DSN。这里改成按行正则匹配，不执行文件内容。
#
# 口令不进命令行参数：用 MYSQL_PWD 传给客户端，避免出现在 ps / 容器
# inspect 之外的历史里；容器回落路径同样走 docker exec -e。

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2

# docker exec 回落时用的容器名候选（与 deploy/docker-compose*.yml 的 service
# 名一致；scripts/seed-demo.sh 用的是 yunshu-stack-mysql）。
CONTAINER_CANDIDATES="yunshu-stack-mysql yunshu-mysql mysql"

# 越界判定：三个条件缺一不可。context_length = 0 是「未知」，没有可比的对象，
# 与 validateMaxOutputTokens 的口径一致，必须排除在外。
readonly OVERFLOW_SQL="SELECT id, model_name, context_length, max_output_tokens FROM t_model WHERE deleted_at IS NULL AND context_length > 0 AND max_output_tokens > context_length ORDER BY id"

die() {
  echo "CHECK FAIL: $*" >&2
  exit 2
}

# read_dsn_from_file 从 env 文件里取出 DB_DSN 的字面值（去掉包裹的引号）。
read_dsn_from_file() {
  local file="$1" line
  [ -f "$file" ] || return 1
  line=$(grep -m1 -E "^[[:space:]]*(export[[:space:]]+)?DB_DSN[[:space:]]*=" "$file") || return 1
  line=${line#*=}
  line=${line#"${line%%[![:space:]]*}"}
  case "$line" in
    \"*\") line=${line#\"}; line=${line%\"} ;;
    \'*\') line=${line#\'}; line=${line%\'} ;;
  esac
  [ -n "$line" ] || return 1
  printf '%s' "$line"
}

# resolve_dsn 取第一个「有值」的 DB_DSN。deploy/.env.stack 里 DB_DSN 通常是
# 空的（容器内用 service 名直连），所以「文件存在」不等于「拿到了 DSN」，必须
# 继续看下一个候选，否则本脚本会在有 .env 的开发机上白跑一趟。
resolve_dsn() {
  local file value
  if [ -n "${YUNSHU_ENV_FILE:-}" ]; then
    read_dsn_from_file "$YUNSHU_ENV_FILE"
    return
  fi
  for file in deploy/.env.stack .env; do
    [ -f "$file" ] || continue
    value=$(read_dsn_from_file "$file") || continue
    [ -n "$value" ] || continue
    printf '%s' "$value"
    return 0
  done
  return 1
}

DSN="${DB_DSN:-}"
if [ -z "$DSN" ]; then
  DSN=$(resolve_dsn) || DSN=""
fi

DB_HOST=""
DB_PORT=""
DB_USER=""
DB_PASS=""
DB_NAME=""
if [ -n "$DSN" ]; then
  # go-sql-driver 的 DSN：user:pass@tcp(host:port)/dbname?params
  # 口令里可能含 @ 或 :，所以 user 段按「第一个冒号前」切，口令段按「最后一个
  # @ 之前」切。
  dsn_re='^([^:]*):(.*)@tcp\(([^:()]+):([0-9]+)\)/([^?]+)'
  if [[ "$DSN" =~ $dsn_re ]]; then
    DB_USER="${BASH_REMATCH[1]}"
    DB_PASS="${BASH_REMATCH[2]}"
    DB_HOST="${BASH_REMATCH[3]}"
    DB_PORT="${BASH_REMATCH[4]}"
    DB_NAME="${BASH_REMATCH[5]}"
  else
    die "无法解析 DB_DSN（期望 user:pass@tcp(host:port)/dbname 形式），请显式设置 DB_DSN 或 MYSQL_* 变量"
  fi
fi

DB_HOST="${MYSQL_HOST:-${DB_HOST:-127.0.0.1}}"
DB_PORT="${MYSQL_PORT:-${DB_PORT:-3306}}"
DB_USER="${MYSQL_USER:-${DB_USER:-root}}"
DB_PASS="${MYSQL_PASSWORD:-${DB_PASS:-}}"
DB_NAME="${YUNSHU_DB_NAME:-${DB_NAME:-yunshu}}"

echo "T48 存量越界巡检：t_model.context_length vs max_output_tokens"
echo "    目标 ${DB_HOST}:${DB_PORT}/${DB_NAME}（只读，仅一条 SELECT）"

# resolve_runner 决定用本机 mysql 客户端还是 docker exec，绝不静默降级：两条
# 路径都不可用时退出 2，避免「查不到」被当成「0 行越界」。
#
# 容器候选只看「正在运行」的：`docker inspect` 对已停止的容器同样返回 0，而
# 这台机器上 yunshu-stack-mysql（compose stack 用）与 yunshu-mysql（本地 dev
# 用）会同时存在，只按名字判断会挑到停着的那个，然后报「not running」。
#
# 名字匹配用 `docker ps --format` 自己比，不用 --filter name=：后者在不同
# Docker 版本里对内层名字带不带前导 `/` 处理不一致（本机 8.x 用 `^/name$` 匹配
# 不到，`^name$` 才行），一次全等比较省掉这个版本坑。
container_running() {
  local name="$1" running
  [ -n "$name" ] || return 1
  running=$(docker ps --filter status=running --format '{{.Names}}' 2>/dev/null) || return 1
  printf '%s\n' "$running" | grep -Fxq "$name"
}

MYSQL_BIN="${MYSQL_BIN:-mysql}"
RUNNER=""
CONTAINER=""
if command -v "$MYSQL_BIN" >/dev/null 2>&1; then
  RUNNER="client"
else
  if command -v docker >/dev/null 2>&1; then
    if [ -n "${YUNSHU_MYSQL_CONTAINER:-}" ]; then
      container_running "$YUNSHU_MYSQL_CONTAINER" && CONTAINER="$YUNSHU_MYSQL_CONTAINER"
    else
      for candidate in $CONTAINER_CANDIDATES; do
        if container_running "$candidate"; then
          CONTAINER="$candidate"
          break
        fi
      done
    fi
  fi
  [ -n "$CONTAINER" ] && RUNNER="docker"
fi

# run_query 执行一条只读 SQL，输出 TSV（无表头）。任何失败都退出 2。
run_query() {
  local sql="$1" out="" status=1
  if [ "$RUNNER" = "client" ]; then
    if [ -n "$DB_PASS" ]; then
      out=$(MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" --protocol=TCP \
        -h "$DB_HOST" -P "$DB_PORT" -u "$DB_USER" -N -B -e "$sql" "$DB_NAME" 2>&1)
      status=$?
    else
      out=$("$MYSQL_BIN" --protocol=TCP \
        -h "$DB_HOST" -P "$DB_PORT" -u "$DB_USER" -N -B -e "$sql" "$DB_NAME" 2>&1)
      status=$?
    fi
  elif [ "$RUNNER" = "docker" ]; then
    # 容器内客户端连容器内的 3306；宿主端口映射在容器内不可见。
    out=$(docker exec -e MYSQL_PWD="$DB_PASS" "$CONTAINER" \
      mysql --protocol=TCP -h 127.0.0.1 -P 3306 -u "$DB_USER" -N -B -e "$sql" "$DB_NAME" 2>&1)
    status=$?
  else
    die "找不到 mysql 客户端，也没有可用的 MySQL 容器（试过 ${CONTAINER_CANDIDATES// /、}）；请安装 mysql 客户端或设置 YUNSHU_MYSQL_CONTAINER"
  fi
  if [ "$status" -ne 0 ]; then
    die "查询失败，未获得任何结果（这不等于「0 行越界」）：${out}"
  fi
  printf '%s' "$out"
}

rows=$(run_query "$OVERFLOW_SQL") || exit 2

# 统计非空行。mysql 客户端在零结果时输出为空；docker exec 也一样。
count=0
if [ -n "$rows" ]; then
  count=$(printf '%s\n' "$rows" | grep -c .)
fi

if [ "$count" -eq 0 ]; then
  echo "共 0 行越界"
  exit 0
fi

printf '%-6s %-32s %14s %18s\n' "id" "model_name" "context_length" "max_output_tokens"
printf '%s\n' "$rows" | while IFS=$'\t' read -r id name ctx_len out_max; do
  printf '%-6s %-32s %14s %18s\n' "$id" "$name" "$ctx_len" "$out_max"
done
echo "共 ${count} 行越界"
echo "    -> 这些行是存量脏状态，脚本不会订正。请对每一行执行一次 PATCH /api/admin/models/{id}，"
echo "       在同一次提交里把 context_length 与 max_output_tokens 一起改到一致（只改 context_length 现在会被 400 拒绝）。"
exit 1
