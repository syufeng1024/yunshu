#!/usr/bin/env bash
# Hard-constraint scans. These encode rules that have already been accepted as
# non-negotiable across T01-T09; a hit here means someone quietly broke one.
#
# This is deliberately NOT a general-purpose linter. golangci-lint belongs in
# T21's CI job; this script must stay dependency-free so `scripts/dev.sh ci`
# works on a machine with nothing but Go installed.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

fail=0

check_empty() {
  local name="$1" hits="$2" hint="$3"
  if [ -n "$hits" ]; then
    echo "LINT FAIL: $name"
    echo "$hits" | sed 's/^/    /' | head -10
    echo "    -> $hint"
    fail=1
  else
    echo "ok: $name"
  fi
}

# 1. encoding/json is funnelled through internal/jsonx for the main module.
#    relaykit is a separate module and may use it (documented exception, T08).
check_empty "encoding/json only in internal/jsonx (relaykit exempt)" \
  "$(grep -rn '"encoding/json"' --include='*.go' internal cmd 2>/dev/null \
     | grep -v 'internal/jsonx' | grep -v '_test\.go')" \
  "import internal/jsonx instead"

# 2. Money never touches floating point. temperature is not money.
check_empty "no float in ledger/billing" \
  "$(grep -rn 'float32\|float64' --include='*.go' internal/ledger internal/billing 2>/dev/null \
     | grep -v '_test\.go')" \
  "amounts are int64 micro-credits"

# 3. Migrations must not use TIMESTAMP: it is timezone-shifting, datetime(3) is not.
check_empty "no timestamp in migrations" \
  "$(grep -rni 'timestamp' internal/platform/migrate/sql/mysql/*.sql 2>/dev/null)" \
  "use datetime(3) storing UTC"

# 4. No foreign keys: consistency is enforced in the application layer.
check_empty "no FOREIGN KEY in migrations" \
  "$(grep -rni 'foreign key' internal/platform/migrate/sql/mysql/*.sql 2>/dev/null)" \
  "the ledger relies on append-only writes, not referential constraints"

# 5. The balance column is derived from the ledger. Nothing may overwrite it.
#    _test.go files may plant a drift on purpose (T21's invariant negative
#    control); production code may not.
check_empty "no direct balance overwrite" \
  "$(grep -rn 'UPDATE t_credit_account SET balance_micro' --include='*.go' internal cmd 2>/dev/null \
     | grep -v '_test\.go')" \
  "append a ledger entry; balance is a materialised view"

# 6. The ledger is append-only.
check_empty "no UPDATE/DELETE on t_ledger_entry" \
  "$(grep -rn 'UPDATE t_ledger_entry\|DELETE FROM t_ledger_entry' --include='*.go' internal cmd 2>/dev/null \
     | grep -v '_test\.go')" \
  "correct mistakes with a REVERSAL entry"

# 7. Licence hygiene: new-api allows secondary development and needs no
#    attribution for referencing its code or architecture (errata E-23). What
#    we must not do is carry its AGPL code into this closed-source product, so
#    the grep looks for the upstream project's identifying string in Go/SQL
#    only — docs and comments may cite the project by name.
check_empty "no new-api source string in Go/SQL (we do not copy AGPL code)" \
  "$(grep -rn 'QuantumNous' --include='*.go' --include='*.sql' \
     relaykit internal cmd 2>/dev/null)" \
  "see TDS §0 licensing note / errata E-23"

# 8. Request paths must inherit the caller's context, never invent one.
check_empty "no context.Background() under internal/" \
  "$(grep -rn 'context.Background()' --include='*.go' internal 2>/dev/null \
     | grep -v '_test\.go' | grep -v 'testmain')" \
  "derive from the request context so cancellation propagates"

# 9. golangci-lint (T21). It is a CI dependency, not a local prerequisite:
#    a developer machine without it must still be able to run `dev.sh ci`.
#    The config is deliberately narrow (govet + ineffassign) so the first
#    adoption does not turn a pre-existing style backlog into a red build.
if command -v golangci-lint >/dev/null 2>&1; then
  if ! golangci-lint run --timeout 5m ./...; then
    echo "LINT FAIL: golangci-lint"
    fail=1
  else
    echo "ok: golangci-lint"
  fi
else
  echo "WARN: golangci-lint not installed; skipped (CI installs it)"
fi

# 10. Files whose exact bytes are hashed must be LF on every platform. A CRLF
#     checkout changes every migration checksum, so a Windows-built image stops
#     matching a CI-built one. check-eol.sh lists the offending paths and the
#     command that repairs them, and is silent when everything is clean.
if eol_out="$(bash scripts/check-eol.sh 2>&1)"; then
  echo "ok: 字节校验文件为 LF"
else
  printf '%s\n' "$eol_out"
  fail=1
fi

# 11. The test DSN/Redis resolver must probe the container ports (13306/16379)
#     before a native local install (3306/6379). GR-01 pins this project to
#     MySQL 8.0 and the container runs exactly that, while a developer machine's
#     3306 is commonly 8.4: probing it first puts the whole suite on the wrong
#     baseline and reports false green. The guard stubs the port prober, so it
#     needs neither live ports, nor MySQL, nor Docker — safe in CI's static job.
if guard_out="$(bash scripts/test-env_test.sh 2>&1)"; then
  echo "ok: test-env 端口优先级（容器 13306/16379 优先）"
else
  printf '%s\n' "$guard_out" | grep -E '^(FAIL  |guard:)' || true
  echo "    -> fix scripts/test-env.sh; do not probe 3306/6379 before 13306/16379"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo
  echo "lint: FAILED"
  exit 1
fi

echo
echo "lint: all hard constraints hold"
