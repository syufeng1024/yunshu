#!/usr/bin/env bash
# stack.sh — 全栈容器化部署入口（无需 GNU make）。
#
# deploy/docker-compose.yml      只跑 MySQL / Redis，应用用 go run 跑在宿主
# deploy/docker-compose.stack.yml 全容器化：存储 + 三进程 + 前端
#
# 本脚本是后者的一键入口，和 Makefile 的 stack-* 目标一一对应，行为一致。
#
#   scripts/stack.sh env [--force]  生成 deploy/.env.stack（随机密钥 + 自动挑空闲端口）
#   scripts/stack.sh build          只构建镜像
#   scripts/stack.sh up             构建并启动，等全部健康
#   scripts/stack.sh down           停止（保留数据卷）
#   scripts/stack.sh ps             查看状态
#   scripts/stack.sh logs [服务名]   跟踪日志
#   scripts/stack.sh smoke          冒烟：转发给 scripts/smoke.sh（有断言与退出码）
#   scripts/stack.sh reset          停服务并删除数据卷（不可恢复，需二次确认）
#
# 端口：宿主 3306/6379 常被本机 MySQL/Redis 占用，所以存储层在本编排里
# 干脆不发布端口。只有 web / gatewayd / controld 三个入口对外，env 生成时
# 会探测占用并顺延到空闲端口，避免和机器上其他项目打架。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STACK_COMPOSE="deploy/docker-compose.stack.yml"
STACK_ENV="deploy/.env.stack"
ENV_EXAMPLE="deploy/.env.stack.example"

WEB_PORT_DEFAULT=8088
GATEWAY_PORT_DEFAULT=18080
CONTROL_PORT_DEFAULT=18081

port_busy() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}\$"
  elif command -v netstat >/dev/null 2>&1; then
    netstat -ano 2>/dev/null | grep -E "[:.]${p}[[:space:]]" | grep -q LISTENING
  else
    return 1
  fi
}

# 从给定起点往上找第一个空闲端口，最多试 30 个。
pick_port() {
  local p="$1"
  local i=0
  while [ "$i" -lt 30 ]; do
    if ! port_busy "$p"; then
      echo "$p"
      return 0
    fi
    p=$((p + 1))
    i=$((i + 1))
  done
  echo "ERROR: 从 $1 起连续 30 个端口都被占用" >&2
  return 1
}

b64_32() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 32 | tr -d '\n'
  else
    head -c 32 /dev/urandom | base64 | tr -d '\n'
  fi
}

hex_16() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16 | tr -d '\n'
  else
    head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# 读取现有 .env.stack 里的某个键
env_get() {
  local key="$1" fallback="$2"
  if [ -f "$STACK_ENV" ]; then
    local v
    v="$(grep -E "^${key}=" "$STACK_ENV" | tail -1 | cut -d= -f2- || true)"
    if [ -n "${v}" ]; then echo "$v"; return 0; fi
  fi
  echo "$fallback"
}

compose() {
  docker compose --env-file "$STACK_ENV" -f "$STACK_COMPOSE" "$@"
}

require_env() {
  if [ ! -f "$STACK_ENV" ]; then
    echo "ERROR: $STACK_ENV 不存在，先跑：scripts/stack.sh env" >&2
    exit 1
  fi
  if ! grep -qE '^SESSION_SECRET=.+' "$STACK_ENV"; then
    echo "ERROR: SESSION_SECRET 未设置，重跑：scripts/stack.sh env --force" >&2
    exit 1
  fi
}

write_env() {
  local force="${1:-}"
  if [ -f "$STACK_ENV" ] && [ "$force" != "--force" ]; then
    echo "$STACK_ENV 已存在，跳过（要重新生成用 env --force，会覆盖口令与密钥）"
    return 0
  fi
  if [ ! -f "$ENV_EXAMPLE" ]; then
    echo "ERROR: $ENV_EXAMPLE 缺失" >&2
    exit 1
  fi

  local web gw ctl
  web="$(pick_port "$WEB_PORT_DEFAULT")"
  gw="$(pick_port "$GATEWAY_PORT_DEFAULT")"
  ctl="$(pick_port "$CONTROL_PORT_DEFAULT")"

  local session kek metrics fp phone
  session="$(b64_32)"
  kek="$(b64_32)"
  metrics="$(hex_16)"
  fp="$(b64_32)"
  phone="$(b64_32)"

  # sed 的替换串里 base64 可能带 / 与 &，用 | 作分隔符并对 & 转义。
  esc() { printf '%s' "$1" | sed -e 's/[&|]/\\&/g'; }

  sed -e "s|^WEB_PORT=.*|WEB_PORT=${web}|" \
      -e "s|^GATEWAY_PORT=.*|GATEWAY_PORT=${gw}|" \
      -e "s|^CONTROL_PORT=.*|CONTROL_PORT=${ctl}|" \
      -e "s|^SESSION_SECRET=.*|SESSION_SECRET=$(esc "$session")|" \
      -e "s|^CHANNEL_KEY_KEK=.*|CHANNEL_KEY_KEK=$(esc "$kek")|" \
      -e "s|^FP_HMAC_KEY=.*|FP_HMAC_KEY=$(esc "$fp")|" \
      -e "s|^PHONE_HMAC_KEY=.*|PHONE_HMAC_KEY=$(esc "$phone")|" \
      -e "s|^GATEWAY_METRICS_TOKEN=.*|GATEWAY_METRICS_TOKEN=${metrics}|" \
      "$ENV_EXAMPLE" > "$STACK_ENV"

  echo "已写入 $STACK_ENV"
  echo "  站点      http://localhost:${web}"
  echo "  网关 API  http://localhost:${gw}"
  echo "  控制面    http://localhost:${ctl}"
  echo "  会话密钥、KEK、FP_HMAC_KEY 与 PHONE_HMAC_KEY 已随机生成；MYSQL_ROOT_PASSWORD 仍是模板里的 dev 值，上线前必须换"
  echo "  有存量手机号账号时，上线后先跑 backfill-phone-hmac detect（见 docs/ops-runbook.md）"
}

smoke() {
  # 断言与退出码统一放在 scripts/smoke.sh：它是可以挂进 CI 的版本，
  # 这里只做转发，避免两套判据各自漂移。
  exec bash "$ROOT/scripts/smoke.sh"
}

cmd="${1:-help}"
shift || true

case "$cmd" in
  env)
    write_env "${1:-}"
    ;;
  build)
    require_env
    compose build
    ;;
  up)
    require_env
    compose up -d --build --wait --wait-timeout 300
    compose ps
    echo
    echo "站点： http://localhost:$(env_get WEB_PORT "$WEB_PORT_DEFAULT")"
    ;;
  down)
    require_env
    compose down
    ;;
  ps)
    require_env
    compose ps
    ;;
  logs)
    require_env
    compose logs -f --tail=100 "$@"
    ;;
  smoke)
    smoke
    ;;
  reset)
    require_env
    echo "⚠️  此操作将删除 mysql_data 与 redis_data 两个数据卷，所有账号、余额、流水不可恢复。"
    printf '确认请输入 yes：'
    read -r answer
    if [ "$answer" != "yes" ]; then
      echo "已取消"
      exit 1
    fi
    compose down -v
    echo "已删除容器与数据卷"
    ;;
  help|*)
    sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'
    ;;
esac
