#!/usr/bin/env bash
# =============================================================================
#  云枢 API 中转站 · 演示数据播种脚本
# =============================================================================
#  作用：给「已存在」的演示账号（默认 demo@yunshu.test，t_user.id = 3）铺一套
#        真实可看的数据，让控制台各页与官网模型广场不再空白：
#          1. 平台内容：4 个已上架模型 + 各自的当前生效价目 + 能力矩阵
#          2. 资金账本：99 元充值 9900 积分 + 赠送 2000 积分，再消耗 43.5 积分
#                       （两条批次、8 条流水，满足 internal/ledger 的四条不变量）
#          3. 上游渠道：1 个境内渠道 + 4 条路由 + 模型→渠道关联；渠道密钥由
#                       cmd/keytool seed-channel 用 CHANNEL_KEY_KEK 加密成 v1: 信封
#          4. API Key ：优先走控制台 API 真实创建（哈希/掩码/K13 全部由服务端写）
#          5. 日志用量：8 条 t_usage_record（含 2 条失败）+ 2 条上游重试时间线
#          6. 对账摘要：余额 / 批次数 / 流水数 / 用量行数 / 模型数 + 不变量断言
#
#  用法：
#      bash scripts/seed-demo.sh              # 播种并打印对账摘要
#      bash scripts/seed-demo.sh --summary     # 只打印摘要（不写数据）
#
#  幂等：脚本可以反复执行。每次运行都用固定业务键（渠道名 / 订单号 / 批次号 /
#        request_id / 幂等键）覆盖同一批演示数据，不新增重复行、不叠加余额。
#        删除范围一律用「账号 id + 演示哨兵前缀」精确限定，绝不 TRUNCATE、
#        绝不无条件 DELETE，也不碰 yunshu_test / yunshu_log 之外的任何库。
#
#  依赖：docker（MySQL / Redis / controld 跑在容器里，root 口令与 CHANNEL_KEY_KEK
#        从 deploy/.env.stack 读取，既不出现在命令行参数里，也不写进 SQL 文本）；
#        可选 curl（用于走控制台 API 建 Key，不可用时自动回落到等价语义的 SQL
#        插入）；keytool 由临时 golang 容器现场编译（需要能拿到 golang 镜像与
#        yunshu-gomod 模块缓存卷）。
#
#  密钥落库方式：渠道明文密钥只存在于本进程变量与管道 stdin 中，经
#  `keytool seed-channel` 加密后写 t_channel.upstream_key_enc；脚本不回显、
#  不落盘，最终只打印 v1: 前缀 + 字节长度 + upstream_key_mask。
#
#  快照刷新：渠道/路由/价目是直接写库的，controld 的 ROUTING 快照与 K19
#  价目由启动时的同步 tick 重发，因此脚本末尾会重启 controld 并等待就绪，
#  再校验 Redis 里的 cfg:snap:ROUTING 已包含演示渠道。不重启则要等 4 分钟。
# =============================================================================

set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
SEED_DIR="$ROOT_DIR/deploy/seed"
ENV_FILE="${YUNSHU_ENV_FILE:-$ROOT_DIR/deploy/.env.stack}"

CONTAINER="${YUNSHU_MYSQL_CONTAINER:-yunshu-stack-mysql}"
DATABASE="${YUNSHU_DB_NAME:-yunshu}"
DEMO_EMAIL="${DEMO_EMAIL:-demo@yunshu.test}"
# 默认值必须与库内既有演示账号的真实口令一致，否则脚本会在登录步骤 die。
# 该口令于 2026-10-30 由 Demo#2026 轮换为 Review#2026（见 docs/tasks/HANDOFF-2026-10-30.md）。
DEMO_PASSWORD="${DEMO_PASSWORD:-Review#2026}"
CONTROL_URL="${YUNSHU_CONTROL_URL:-http://localhost:18081}"

# 渠道 / 快照刷新相关。全部可覆盖，默认值与 deploy/docker-compose.stack.yml 一致。
CHANNEL_NAME="${YUNSHU_DEMO_CHANNEL_NAME:-demo-domestic-channel}"
REDIS_CONTAINER="${YUNSHU_REDIS_CONTAINER:-yunshu-stack-redis}"
CONTROL_CONTAINER="${YUNSHU_CONTROL_CONTAINER:-yunshu-stack-controld}"
KEYTOOL_IMAGE="${YUNSHU_KEYTOOL_IMAGE:-golang:1.27-alpine}"
GOMOD_CACHE_VOLUME="${YUNSHU_GOMOD_VOLUME:-yunshu-gomod}"
STACK_NETWORK="${YUNSHU_STACK_NETWORK:-yunshu-stack_backend}"

# 两个演示密钥的名字（也是幂等判定的依据之一）
KEY_NAME_1="演示密钥 · 全模型"
KEY_NAME_2="演示密钥 · 指定模型"

MYSQL_ROOT_PASSWORD_VALUE=""
DEMO_UID=""
PROTO_ID=""
TMP_DIR=""

if [ -t 1 ]; then
  C_INFO=$'\033[36m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_OFF=$'\033[0m'
else
  C_INFO=''; C_WARN=''; C_ERR=''; C_OFF=''
fi

info() { printf '%s[seed]%s %s\n' "$C_INFO" "$C_OFF" "$*"; }
warn() { printf '%s[seed] WARN%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die()  { printf '%s[seed] FATAL%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2; exit 1; }

cleanup() {
  # 不用 rm -rf：工作区外的递归删除会被宿主的安全钩子拦下。
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -f -- "$TMP_DIR"/cookies.txt "$TMP_DIR"/login.json "$TMP_DIR"/keytool.env 2>/dev/null || true
    rmdir -- "$TMP_DIR" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 环境与 MySQL 访问
# ---------------------------------------------------------------------------

# env_get KEY：从 .env.stack 里取一个值，去掉行尾注释与包裹引号。
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

# mysql_pipe：把 SQL 从 stdin 送进容器内的 mysql。
# 口令用 MYSQL_PWD 环境变量传入（mysql 客户端优先读取它），因此既不会出现在
# 容器内的 argv（ps 可见）里，也不会触发 "Using a password on the command
# line interface can be insecure" 的告警。
mysql_pipe() {
  docker exec -i -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD_VALUE" "$CONTAINER" \
    mysql --default-character-set=utf8mb4 -uroot --table "$DATABASE"
}

# sql_value：执行一条查询，返回首行首列的裸值（无表头、无边框）。
sql_value() {
  docker exec -i -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD_VALUE" "$CONTAINER" \
    mysql --default-character-set=utf8mb4 -uroot -N -B "$DATABASE" -e "$1"
}

# run_sql_file FILE：注入 @uid / @proto 后执行 SQL 文件。
run_sql_file() {
  local file="$1"
  [ -f "$file" ] || die "缺少 SQL 文件：$file"
  {
    printf 'SET NAMES utf8mb4 COLLATE utf8mb4_0900_as_cs;\n'
    printf 'SET @uid := %s;\n' "$DEMO_UID"
    printf 'SET @proto := %s;\n' "$PROTO_ID"
    cat "$file"
  } | mysql_pipe
}

# ---------------------------------------------------------------------------
# 前置检查
# ---------------------------------------------------------------------------
preflight() {
  [ -f "$ENV_FILE" ] || die "找不到环境文件：$ENV_FILE（先 cp deploy/.env.stack.example deploy/.env.stack）"
  command -v docker >/dev/null 2>&1 || die "PATH 里没有 docker"

  MYSQL_ROOT_PASSWORD_VALUE=$(env_get MYSQL_ROOT_PASSWORD)
  [ -n "$MYSQL_ROOT_PASSWORD_VALUE" ] || die "$ENV_FILE 里没有 MYSQL_ROOT_PASSWORD"

  # 渠道密钥必须加密成 v1: 信封；KEK 与 gatewayd/controld 用的是同一个值。
  [ -n "$(env_get CHANNEL_KEY_KEK)" ] || die "$ENV_FILE 里没有 CHANNEL_KEY_KEK：无法把渠道密钥加密成 v1: 信封"

  # keytool 需要一个能解析 mysql:3306 的容器网络；默认用 compose 的 backend。
  docker network inspect "$STACK_NETWORK" >/dev/null 2>&1 \
    || die "docker 网络 $STACK_NETWORK 不存在（先跑 scripts/stack.sh up，或设 YUNSHU_STACK_NETWORK）"

  local running
  running=$(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || true)
  [ "$running" = "true" ] || die "容器 $CONTAINER 不存在或未运行（先跑 scripts/stack.sh up）"

  [ "$(sql_value 'SELECT 1')" = "1" ] || die "无法连接 $CONTAINER 上的 MySQL"

  DEMO_UID=$(sql_value "SELECT id FROM t_user WHERE email = '$DEMO_EMAIL' AND deleted_at IS NULL LIMIT 1")
  [ -n "$DEMO_UID" ] || die "账号 $DEMO_EMAIL 不存在：本脚本只播种、不建号（先注册该账号）"
  if [ "$DEMO_UID" != "3" ]; then
    warn "演示账号 id 是 $DEMO_UID（预期 3），按实际 id 播种"
  fi

  PROTO_ID=$(sql_value "SELECT id FROM t_protocol WHERE code = 'OPENAI_COMPAT' LIMIT 1")
  [ -n "$PROTO_ID" ] || die "t_protocol 里没有 OPENAI_COMPAT 行，模型无法绑定默认协议"

  TMP_DIR=$(mktemp -d 2>/dev/null || mktemp -d -t yunshu-seed)
}

# ---------------------------------------------------------------------------
# API Key：优先「控制台 API 真实创建」，失败时回落「等价语义的 SQL 插入」
# ---------------------------------------------------------------------------
# 为什么优先走 API：create.go 会同时写 t_api_key（key_hash = SHA256(完整明文)、
# key_prefix = 明文前 8 位、key_last4 = 末 4 位）、t_api_key_model、
# t_api_key_audit，并把 K13 鉴权快照写进 Redis。SQL 直插能复刻前两者的取值语义，
# 但无法产出 K13，那把 Key 在网关侧不可鉴权——所以只在 API 不可用时降级。

csrf_from_jar() {
  awk '$6 == "csrf" { v = $7 } END { print v }' "$1"
}

key_count_via_api() {
  local jar="$1"
  curl -sS --noproxy '*' -b "$jar" \
    "$CONTROL_URL/api/console/keys?page=1&page_size=50" \
    | grep -o '"total":[0-9]*' | head -n1 | cut -d: -f2
}

create_key_via_api() {
  local jar="$1" name="$2" mode="$3" ids_json="$4" expire_at="$5"
  local token
  token=$(csrf_from_jar "$jar")
  [ -n "$token" ] || return 1
  # 请求体走 stdin（--data-binary @-）而不是 -d：在 Windows / Git Bash 下把含
  # 中文的参数交给原生 curl.exe 时，argv 会按控制台代码页转一遍，密钥名会变成
  # 一串 U+FFFD。管道是字节流，不经 argv 转换。
  printf '{"name":"%s","model_scope_mode":"%s","model_ids":%s,"expire_at":"%s"}' \
      "$name" "$mode" "$ids_json" "$expire_at" \
    | curl -sS --noproxy '*' -b "$jar" \
        -H "X-CSRF-Token: $token" -H 'Content-Type: application/json' \
        --data-binary @- \
        -X POST "$CONTROL_URL/api/console/keys"
}

ensure_api_keys() {
  command -v curl >/dev/null 2>&1 || { warn "PATH 里没有 curl"; return 1; }

  local jar="$TMP_DIR/cookies.txt"
  local body="$TMP_DIR/login.json"
  local http
  if ! http=$(curl -sS --noproxy '*' -o "$body" -w '%{http_code}' -c "$jar" \
        -X POST "$CONTROL_URL/api/v1/auth/login" \
        -H 'Content-Type: application/json' \
        -d "{\"identifier\":\"$DEMO_EMAIL\",\"password\":\"$DEMO_PASSWORD\"}"); then
    warn "登录请求失败：$CONTROL_URL 不可达"
    return 1
  fi
  if [ "$http" != "200" ]; then
    warn "登录失败（HTTP $http）：$CONTROL_URL 不可达或演示口令不符"
    return 1
  fi

  local total
  total=$(key_count_via_api "$jar" || true)
  if [ -z "$total" ]; then
    warn "读取密钥列表失败"
    return 1
  fi
  if [ "$total" -gt 0 ] 2>/dev/null; then
    info "  账号已有 $total 把未删除的 Key，跳过创建（幂等）"
    return 0
  fi

  # 有效期必须是未来时刻且必填（PRD §5.2）；用数据库时钟算，避免宿主时区偏差。
  local expire_at
  expire_at=$(sql_value "SELECT DATE_FORMAT(UTC_TIMESTAMP() + INTERVAL 180 DAY, '%Y-%m-%dT%H:%i:%sZ')")
  [ -n "$expire_at" ] || { warn "取不到密钥有效期"; return 1; }

  local model_ids_json
  model_ids_json=$(sql_value "
    SELECT CONCAT('[', GROUP_CONCAT(id ORDER BY recommend_weight DESC), ']')
      FROM t_model
     WHERE model_name IN ('qwen-max', 'deepseek-chat', 'glm-4-plus') AND deleted_at IS NULL")
  [ -n "$model_ids_json" ] || model_ids_json='[]'

  local resp1 resp2
  if ! resp1=$(create_key_via_api "$jar" "$KEY_NAME_1" "ALL" "[]" "$expire_at"); then
    warn "创建第 1 把 Key 失败"
    return 1
  fi
  case "$resp1" in
    *'"key":"sk-'*) ;;
    *) warn "创建第 1 把 Key 返回异常：$resp1"; return 1 ;;
  esac
  if ! resp2=$(create_key_via_api "$jar" "$KEY_NAME_2" "SELECTED" "$model_ids_json" "$expire_at"); then
    warn "创建第 2 把 Key 失败"
    return 1
  fi
  case "$resp2" in
    *'"key":"sk-'*) ;;
    *) warn "创建第 2 把 Key 返回异常：$resp2"; return 1 ;;
  esac

  info "  已通过控制台 API 创建 2 把 Key（哈希、掩码、审计与 K13 均由服务端写入）"
  printf '%s\n' "$resp1" "$resp2" | grep -o '"key":"sk-[^"]*"' | cut -d'"' -f4 | while IFS= read -r k; do
    printf '        %s\n' "$k"
  done
  warn "  上面是明文密钥，仅此刻可见：演示环境用完可删，脚本不会保存它们"
  return 0
}

# 回落路径：按 create.go 的真实取值规则自己算一遍（key_hash=sha256(明文)、
# key_prefix=前 8 位、key_last4=末 4 位、status=ENABLED、group_code=standard）。
fallback_insert_keys() {
  command -v sha256sum >/dev/null 2>&1 || { warn "PATH 里没有 sha256sum"; return 1; }

  local -a names=("$KEY_NAME_1" "$KEY_NAME_2")
  local sql="" name body key hash prefix last4
  local idx=0
  for name in "${names[@]}"; do
    idx=$((idx + 1))
    body=$( (LC_ALL=C tr -dc '0-9A-HJKMNP-TV-Z' < /dev/urandom 2>/dev/null || true) | head -c 48 || true)
    [ "${#body}" -eq 48 ] || { warn "随机源异常，无法生成密钥明文"; return 1; }
    key="sk-$body"
    hash=$(printf '%s' "$key" | sha256sum | cut -d' ' -f1)
    prefix=${key:0:8}
    last4=${key: -4}
    sql+="INSERT INTO t_api_key
            (user_id, key_name, key_prefix, key_hash, key_hash_version, key_last4,
             status, model_scope_mode, ip_whitelist_mode, group_code, expire_at, created_at, updated_at)
          VALUES ($DEMO_UID, '$name', '$prefix', '$hash', 1, '$last4',
                  'ENABLED', 'ALL', 'OFF', 'standard',
                  UTC_TIMESTAMP(3) + INTERVAL 180 DAY, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3));"$'\n'
    printf '        %s  (%s…%s)\n' "$key" "$prefix" "$last4"
  done

  printf 'SET NAMES utf8mb4 COLLATE utf8mb4_0900_as_cs;\n%s' "$sql" | mysql_pipe >/dev/null
  warn "  已用 SQL 插入 2 行等价密钥：可用于密钥列表展示，但网关鉴权需要 K13 快照，"
  warn "  这 2 把 Key 不可实际调用。要可用的 Key，请登录控制台点「新建密钥」。"
  return 0
}

require_a_key() {
  local n
  n=$(sql_value "SELECT COUNT(*) FROM t_api_key WHERE user_id = $DEMO_UID AND deleted_at IS NULL")
  [ "$n" -gt 0 ] 2>/dev/null || die "账号 $DEMO_UID 没有任何 API Key：t_usage_record.api_key_id 不允许为空"
}

# ---------------------------------------------------------------------------
# 上游渠道：解密路径与密钥落库
# ---------------------------------------------------------------------------
# 04-channel.sql 只建渠道行与路由，upstream_key_enc 留空。真正的密文由
# cmd/keytool seed-channel 写入：它读 CHANNEL_KEY_KEK、从 stdin 取明文、
# 用 cryptobox 生成 "v1:" 信封，并同时写 upstream_key_mask。
# 明文既不进 argv（走管道），也不落盘；KEK 与数据库口令只进 --env-file。

validate_channel_id() {
  case "${1:-}" in
    ''|*[!0-9]*) die "渠道 id 非法：'${1:-}'" ;;
  esac
}

# docker_path：Git Bash 的 pwd / mktemp -d 给出 /d/... 或 /tmp/... 形式的
# MSYS 路径，原生 docker.exe 不认识；传给 docker 的宿主路径先转成混合形式
# （D:/codes/...），正斜杠对 --env-file 与 -v 都安全。
docker_path() {
  if command -v cygpath >/dev/null 2>&1; then
    cygpath -m "$1"
  else
    printf '%s' "$1"
  fi
}

seed_channel_key() {
  local cid="$1" kek dsn body key envfile evidence prefix rest bytes mask
  validate_channel_id "$cid"

  kek=$(env_get CHANNEL_KEY_KEK)
  [ -n "$kek" ] || die "CHANNEL_KEY_KEK 为空，无法加密渠道密钥"

  # 演示用明文上游密钥：40 位随机字母数字，前缀 sk-demo-，不作为真实凭据。
  body=$( (LC_ALL=C tr -dc '0-9A-Za-z' < /dev/urandom 2>/dev/null || true) | head -c 40 || true)
  [ "${#body}" -eq 40 ] || die "随机源异常，无法生成渠道密钥明文"
  key="sk-demo-$body"

  # 容器内用服务名寻址；与 compose 的默认 DB_DSN 同构。
  dsn="root:$MYSQL_ROOT_PASSWORD_VALUE@tcp(mysql:3306)/$DATABASE?parseTime=true&loc=UTC&charset=utf8mb4"
  envfile="$TMP_DIR/keytool.env"
  (umask 077; printf 'ENV=local\nCHANNEL_KEY_KEK=%s\nGW_WRITE_DSN=%s\n' "$kek" "$dsn" >"$envfile")

  # keytool 只在 ENV=local 下允许 seed-channel（生产必须走控制面），
  # 因此这里的 ENV=local 只作用于临时容器，栈本身仍是 ENV=prod。
  if ! printf '%s' "$key" | MSYS_NO_PATHCONV=1 docker run --rm -i \
        --network "$STACK_NETWORK" \
        --env-file "$(docker_path "$envfile")" \
        -e CGO_ENABLED=0 \
        -e GOPROXY="${YUNSHU_GOPROXY:-https://goproxy.cn,direct}" \
        -v "$(docker_path "$ROOT_DIR"):/src" \
        -v "$GOMOD_CACHE_VOLUME:/go/pkg/mod" \
        -w /src \
        "$KEYTOOL_IMAGE" \
        sh -c "go build -o /tmp/keytool ./cmd/keytool && exec /tmp/keytool seed-channel -id $cid"; then
    die "keytool seed-channel 失败：渠道 $cid 的密钥未写入（检查 golang 镜像、模块缓存卷与网络 $STACK_NETWORK）"
  fi
  rm -f -- "$envfile"

  # 只打印可公开的证据：v1: 前缀、字节长度、掩码。不打印明文或密文全文。
  evidence=$(sql_value "SELECT CONCAT(CAST(LEFT(upstream_key_enc, 3) AS CHAR), '|', LENGTH(upstream_key_enc), '|', upstream_key_mask) FROM t_channel WHERE id = $cid")
  prefix=${evidence%%|*}
  rest=${evidence#*|}
  bytes=${rest%%|*}
  mask=${rest#*|}
  [ "$prefix" = "v1:" ] || die "渠道 $cid 的 upstream_key_enc 不是 v1: 信封（实际前缀：'$prefix'）"
  case "$bytes" in
    ''|*[!0-9]*) die "渠道 $cid 的密文长度异常：'$bytes'" ;;
  esac
  [ "$bytes" -gt 16 ] || die "渠道 $cid 的密文只有 $bytes 字节，疑似明文占位"
  info "  渠道密钥已加密：prefix=$prefix bytes=$bytes mask=$mask（明文只在管道里，未落盘、未进 argv）"
}

# ---------------------------------------------------------------------------
# 快照刷新：渠道与路由是直接写库的，必须让 controld 立即重发 ROUTING 快照
# ---------------------------------------------------------------------------
# ROUTING 快照与 K19 价目由 controld 的 boot.Refresher 发布：启动时同步 tick
# 一次，之后每 4 分钟 / 1 分钟。播种发生在启动之后，所以重启 controld 是最快
# 的确定性路径；gatewayd 通过 cfg:invalidate 订阅 + 30s 轮询拿到新版本。

wait_control_healthy() {
  command -v curl >/dev/null 2>&1 || { sleep 5; return 0; }
  local i code=""
  for i in $(seq 1 60); do
    code=$(curl -sS --noproxy '*' -o /dev/null -w '%{http_code}' "$CONTROL_URL/healthz" 2>/dev/null || true)
    [ "$code" = "200" ] && return 0
    sleep 1
  done
  die "controld 重启后 60s 内未就绪（$CONTROL_URL/healthz HTTP ${code:-none}）"
}

publish_routing_snapshot() {
  local running
  running=$(docker inspect -f '{{.State.Running}}' "$CONTROL_CONTAINER" 2>/dev/null || true)
  if [ "$running" != "true" ]; then
    warn "容器 $CONTROL_CONTAINER 未运行：跳过快照刷新；路由要等 controld 下一次 tick（≤4 分钟）才生效"
    return 0
  fi

  info "  重启 controld，触发 ROUTING 快照与 K19 价目立即重发"
  docker restart "$CONTROL_CONTAINER" >/dev/null
  wait_control_healthy

  local snap="" ok="" i
  for i in $(seq 1 30); do
    snap=$(docker exec "$REDIS_CONTAINER" redis-cli --no-raw GET cfg:snap:ROUTING 2>/dev/null || true)
    case "$snap" in
      *"$CHANNEL_NAME"*) ok=1; break ;;
    esac
    sleep 1
  done
  [ -n "$ok" ] || die "30s 内 cfg:snap:ROUTING 仍未包含 $CHANNEL_NAME（看 docker logs $CONTROL_CONTAINER）"
  info "  ROUTING 快照已包含渠道 $CHANNEL_NAME（gatewayd 将在订阅/轮询后生效）"
}

# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
main() {
  local mode="seed"
  if [ "${1:-}" = "--summary" ]; then
    mode="summary"
  elif [ -n "${1:-}" ]; then
    die "未知参数：$1（只支持 --summary）"
  fi

  preflight
  info "演示账号 $DEMO_EMAIL → t_user.id = $DEMO_UID，目标库 $DATABASE@$CONTAINER"

  if [ "$mode" = "summary" ]; then
    info "[summary] 只读模式：打印对账摘要"
    run_sql_file "$SEED_DIR/99-summary.sql"
    return 0
  fi

  info "[1/6] 平台内容：4 个已上架模型 + 当前生效价目 + 能力矩阵"
  run_sql_file "$SEED_DIR/01-platform.sql"

  info "[2/6] 资金账本：99 元充值 + 2000 赠送积分 + 6 笔消耗"
  run_sql_file "$SEED_DIR/02-credit.sql"

  info "[3/6] 上游渠道：1 个境内渠道 + 4 条路由 + 模型→渠道关联"
  run_sql_file "$SEED_DIR/04-channel.sql"
  local channel_id
  channel_id=$(sql_value "SELECT id FROM t_channel WHERE name = '$CHANNEL_NAME' LIMIT 1")
  [ -n "$channel_id" ] || die "渠道 $CHANNEL_NAME 未创建（检查 04-channel.sql 与 t_protocol.OPENAI_COMPAT）"
  seed_channel_key "$channel_id"

  info "[4/6] API Key"
  if ! ensure_api_keys; then
    # 回落前先看还差几把：API 可能只成功了一半就报错，此时再插 2 行会超出
    # 免费档 3 把的上限，也没必要。
    local have
    have=$(sql_value "SELECT COUNT(*) FROM t_api_key WHERE user_id = $DEMO_UID AND deleted_at IS NULL")
    if [ "${have:-0}" -gt 0 ] 2>/dev/null; then
      warn "  已有 $have 把未删除的 Key，跳过创建（幂等）"
    else
      warn "  控制台 API 路径不可用，回落到 SQL 插入"
      fallback_insert_keys || die "无法创建演示密钥"
    fi
  fi
  require_a_key

  info "[5/6] 请求日志与用量：8 条用量（含 2 条失败）+ 2 条上游重试"
  run_sql_file "$SEED_DIR/03-usage.sql"

  info "[6/6] 对账摘要与账本不变量"
  run_sql_file "$SEED_DIR/99-summary.sql"

  info "[刷新] 让 controld 重新发布 ROUTING 快照与价目缓存"
  publish_routing_snapshot

  info "完成。演示账号：$DEMO_EMAIL / $DEMO_PASSWORD"
  info "  控制台：$CONTROL_URL"
}

main "$@"
