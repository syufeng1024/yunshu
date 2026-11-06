#!/usr/bin/env bash
# check-openapi-routes.sh — admin 路由与 openapi 契约的双向覆盖闸门（T49）。
#
# 为什么需要这道闸：契约是手写的，路由是代码注册的，两者没有任何机制强制同步。
# T46 收尾时实测过：server.go 注册 64 条 admin 路由，openapi 只登记 19 条，缺 45
# 条，而没有任何测试/CI 会因为这件事失败——前端只能手写类型，契约与实现长期脱节。
# 补一次没有意义，必须有东西在下次新增路由时喊一声。
#
# 比什么（两个方向都算失败）：
#   缺失  代码注册了 METHOD /path，openapi 的 paths 段里没有这个 method
#   多余  openapi 登记了 METHOD /path，代码里没有这条路由
# 任一方向非空 → 退出码 1。全量对齐 → 退出 0，并打印覆盖条数。
#
# 只读：不改任何文件，不跑 gen，不碰 git。
#
# 退出码：
#   0  两个方向都为空（全量覆盖）
#   1  有缺失或有多余
#   2  解析失败（找不到 paths 段 / 一条 operation 都没解析出来 / 源码里没有
#      admin 路由）——「解析不出来」不能冒充「全量覆盖」，所以绝不返回 0
#
# 为什么用 python3 的逐行解析而不是 PyYAML：CI 只需要 Go + node，不保证装了
# PyYAML；本仓库的 openapi.yaml 是自己维护的、缩进固定（paths 下路径键 2 空格、
# method 键 4 空格），逐行解析足够，而且没有第三方依赖。代价是格式假设硬编码在
# 这里，所以解析函数带断言：解析出的 operation 数必须非空，否则按解析失败退出 2，
# 而不是安静地报「0 缺失」。
#
# 用法（仓库根目录，或任意目录——脚本自己 cd 到仓库根）：
#   bash scripts/check-openapi-routes.sh
#   bash scripts/check-openapi-routes.sh --quiet   # 只打印结论与失败项

set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2

OPENAPI="api/openapi.yaml"
SERVER="internal/control/server.go"
QUIET=0
case "${1:-}" in
  --quiet|-q) QUIET=1 ;;
  ''|--help|-h)
    if [ "${1:-}" != "" ]; then
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
    fi
    ;;
  *) echo "未知参数：$1（支持 --quiet）" >&2; exit 2 ;;
esac

PYTHON="${PYTHON:-python3}"
command -v "$PYTHON" >/dev/null 2>&1 || PYTHON=python
command -v "$PYTHON" >/dev/null 2>&1 || {
  echo "CHECK FAIL: 找不到 python3/python，无法解析 $OPENAPI" >&2
  exit 2
}

# 用环境变量传参，避免把路径拼进脚本正文（$ 与引号都不需要转义）。
QUIET="$QUIET" OPENAPI="$OPENAPI" SERVER="$SERVER" "$PYTHON" - <<'PY'
import os
import re
import sys

openapi_path = os.environ["OPENAPI"]
server_path = os.environ["SERVER"]
quiet = os.environ.get("QUIET") == "1"

# ---------------------------------------------------------------------------
# 1. 源码侧：mux.Handle("METHOD /api/admin...") 的每一条
# ---------------------------------------------------------------------------
ROUTE_RE = re.compile(r'mux\.Handle\("([A-Z]+) (/api/admin[^"]*)"')
routes = set()
try:
    with open(server_path, encoding="utf-8") as fh:
        for line in fh:
            for method, path in ROUTE_RE.findall(line):
                routes.add((method.lower(), path))
except OSError as exc:
    print("CHECK FAIL: 读不到 %s：%s" % (server_path, exc), file=sys.stderr)
    sys.exit(2)

# ---------------------------------------------------------------------------
# 2. 契约侧：paths 段。路径键 2 空格缩进且以 / 开头，method 键 4 空格缩进。
#    遇到下一个顶层键（非注释、顶格）就结束 paths 段。
# ---------------------------------------------------------------------------
PATH_RE = re.compile(r'^  (/\S*):\s*$')
METHOD_RE = re.compile(r'^    (get|post|put|patch|delete|head|options):\s*$')
TOP_KEY_RE = re.compile(r'^[A-Za-z_][A-Za-z0-9_-]*:\s*$')

operations = set()
in_paths = False
current = None
try:
    with open(openapi_path, encoding="utf-8") as fh:
        for line in fh:
            line = line.rstrip("\n")
            if not in_paths:
                if line.strip() == "paths:":
                    in_paths = True
                continue
            if TOP_KEY_RE.match(line):
                break
            m = PATH_RE.match(line)
            if m:
                current = m.group(1)
                continue
            m = METHOD_RE.match(line)
            if m and current:
                operations.add((m.group(1), current))
except OSError as exc:
    print("CHECK FAIL: 读不到 %s：%s" % (openapi_path, exc), file=sys.stderr)
    sys.exit(2)

# 解析断言：任何一侧为空都说明解析假设失效（或源码里真的没有 admin 路由），
# 这种情况必须报出来，不能算「全量覆盖」。
if not routes:
    print("CHECK FAIL: %s 里一条 admin 路由都没解析到（正则或文件结构变了？）" % server_path,
          file=sys.stderr)
    sys.exit(2)
if not operations:
    print("CHECK FAIL: %s 的 paths 段没解析出任何 operation（缩进假设：路径 2 空格、method 4 空格）"
          % openapi_path, file=sys.stderr)
    sys.exit(2)

missing = sorted(routes - operations)
extra = sorted(operations - routes)

# 只统计 admin 面的覆盖，非 admin 操作（console/v1）在本卡范围外，但已登记的
# 也算进 operations 集合，因此在做差集前先把非 admin 的排掉，避免「多余」误报。
extra = [op for op in extra if op[1].startswith("/api/admin")]

if not quiet:
    print("admin 路由（%s）：%d 条" % (server_path, len(routes)))
    print("openapi 已登记 admin operation：%d 条"
          % len([op for op in operations if op[1].startswith("/api/admin")]))

if missing:
    print("缺失（代码有、契约无）：%d 条" % len(missing))
    for method, path in missing:
        print("    %-6s %s" % (method.upper(), path))
if extra:
    print("多余（契约有、代码无）：%d 条" % len(extra))
    for method, path in extra:
        print("    %-6s %s" % (method.upper(), path))

if missing or extra:
    print("OPENAPI FAIL: 缺失 %d 条 / 多余 %d 条" % (len(missing), len(extra)))
    sys.exit(1)

print("admin 路由 %d 条，openapi 全部覆盖" % len(routes))
PY
status=$?
case "$status" in
  0) exit 0 ;;
  1) exit 1 ;;
  *) exit 2 ;;
esac
