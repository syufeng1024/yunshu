#!/usr/bin/env bash
# Local-only e2e backend boot: fresh yunshu_e2e schema + controld in Docker.
#
# Run from Git Bash:
#   bash scripts/e2e-backend.sh
#   cd web && npx playwright test
#
# The Playwright config reuses an already-healthy :8081, so this script is only
# for the local "no go toolchain" machine. CI starts controld through the
# config's second webServer (`go run ./cmd/controld`).
set -euo pipefail

export PATH="/c/Users/EDY/AppData/Local/Programs/DockerDesktop/resources/bin:$PATH"
export MSYS_NO_PATHCONV=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && (pwd -W 2>/dev/null || pwd))"
GO_IMG="${GO_IMG:-golang:1.27-alpine}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-yunshu-mysql}"
MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-root}"
MYSQL_PORT="${MYSQL_PORT:-13306}"
REDIS_PORT="${REDIS_PORT:-16379}"
DB="${E2E_DB_NAME:-yunshu_e2e}"
DSN="root:${MYSQL_ROOT_PASSWORD}@tcp(host.docker.internal:${MYSQL_PORT})/${DB}?parseTime=true&loc=UTC&charset=utf8mb4"
KEK="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

echo "==> recreate ${DB} (development schema yunshu is untouched)"
docker exec "${MYSQL_CONTAINER}" mysql -uroot -p"${MYSQL_ROOT_PASSWORD}" -e "
  DROP DATABASE IF EXISTS \`${DB}\`;
  CREATE DATABASE \`${DB}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_as_cs;"

echo "==> migrate ${DB}"
docker run --rm \
  -v "${ROOT}:/src" -v yunshu-gomod:/go/pkg/mod -w /src \
  -e ENV=dev -e DB_DSN="${DSN}" -e LOG_SQL_DSN="${DSN}" \
  "${GO_IMG}" sh -c "go run ./cmd/migrate up"

echo "==> start controld on :8081"
docker rm -f yunshu-e2e-controld >/dev/null 2>&1 || true
docker run -d --name yunshu-e2e-controld \
  -p 8081:8081 \
  -v "${ROOT}:/src" -v yunshu-gomod:/go/pkg/mod -w /src \
  -e ENV=dev \
  -e DB_DSN="${DSN}" \
  -e LOG_SQL_DSN="${DSN}" \
  -e REDIS_ADDR="host.docker.internal:${REDIS_PORT}" \
  -e SESSION_SECRET="e2e-only-session-secret-change-before-deploy" \
  -e CONTROL_HTTP_ADDR=":8081" \
  -e CHANNEL_KEY_KEK="${KEK}" \
  -e FP_HMAC_KEY="e2e-only-fp-hmac-key" \
  -e PHONE_HMAC_KEY="e2e-only-phone-hmac-key-change-before-use" \
  -e EXPORT_HMAC_KEY="e2e-only-export-hmac-key" \
  "${GO_IMG}" sh -c "go run ./cmd/controld"

for _ in $(seq 1 180); do
  if curl -fsS http://127.0.0.1:8081/healthz >/dev/null 2>&1; then
    echo "==> controld ready"
    echo "E2E_MYSQL_CONTAINER=${MYSQL_CONTAINER}"
    echo "E2E_MYSQL_USER=root"
    echo "E2E_MYSQL_PASSWORD=${MYSQL_ROOT_PASSWORD}"
    echo "E2E_DB_NAME=${DB}"
    exit 0
  fi
  sleep 1
done

echo "controld did not become healthy" >&2
docker logs --tail 120 yunshu-e2e-controld >&2 || true
exit 1
