SHELL := /bin/sh
COMPOSE := docker compose -f deploy/docker-compose.yml
STACK := docker compose --env-file deploy/.env.stack -f deploy/docker-compose.stack.yml
GO ?= go
LOAD_ENV = set -e; set -a; if [ -f .env ]; then . ./.env; fi; set +a;

.PHONY: build test vet lint up down migrate run-gw run-ctl run-jobs check \
        stack-env stack-up stack-build stack-down stack-ps stack-logs stack-smoke \
        backfill-phone-hmac backfill-phone-claim

build:
	$(GO) build ./...

test:
	$(GO) test -p 2 ./...

vet:
	$(GO) vet ./...

lint:
	@files="$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))"; \
	if [ -n "$$files" ]; then echo "gofmt required:"; echo "$$files"; exit 1; fi
	$(GO) vet ./...

up:
	$(COMPOSE) up -d --wait --wait-timeout 180

down:
	$(COMPOSE) down

migrate:
	@$(LOAD_ENV) $(GO) run ./cmd/migrate up

# 手机号 hmac 回填。默认只跑只读的 detect；写库必须显式给 ARGS：
#   make backfill-phone-hmac                      # detect（只读，退出码 2/3 表示要人工裁决）
#   make backfill-phone-hmac ARGS="apply"         # dry-run，不写库
#   make backfill-phone-hmac ARGS="apply --commit"
#   make backfill-phone-hmac ARGS="verify"
# 需要 .env 里的 DB_DSN 与 PHONE_HMAC_KEY；四步次序见 docs/ops-runbook.md。
ARGS ?= detect
backfill-phone-hmac:
	@$(LOAD_ENV) $(GO) run ./cmd/backfill-phone-hmac $(ARGS)

# 号码台账（t_phone_claim）回填与释放。两个动词互不隐含，顺序是硬约束：
#   make backfill-phone-claim                       # detect（只读）
#   make backfill-phone-claim ARGS="backfill"        # dry-run
#   make backfill-phone-claim ARGS="backfill --commit"
#   make backfill-phone-claim ARGS="release --commit"
#   make backfill-phone-claim ARGS="verify"
#   make backfill-phone-claim ARGS="lookup --phone=13800138000"
# 先 backfill 再 release：顺序反了会给老注销号白送一次体验版（release 会以退出码 3
# 拒绝执行）。需要 .env 里的 DB_DSN；只有 lookup 需要 PHONE_HMAC_KEY。
backfill-phone-claim:
	@$(LOAD_ENV) $(GO) run ./cmd/backfill-phone-claim $(ARGS)

run-gw:
	@$(LOAD_ENV) $(GO) run ./cmd/gatewayd

run-ctl:
	@$(LOAD_ENV) $(GO) run ./cmd/controld

run-jobs:
	@$(LOAD_ENV) $(GO) run ./cmd/jobsd

check: vet test

# 全栈容器化部署：MySQL + Redis + 三进程 + 前端，一条命令起完整站点。
# 与上面的 up/down（只跑依赖）互不影响，project name 不同。
stack-env:
	@if [ -f deploy/.env.stack ]; then echo "deploy/.env.stack 已存在，跳过"; \
	else cp deploy/.env.stack.example deploy/.env.stack; \
	  sed -i.bak "s|^SESSION_SECRET=$$|SESSION_SECRET=$$(openssl rand -base64 32)|" deploy/.env.stack; \
	  sed -i.bak "s|^CHANNEL_KEY_KEK=$$|CHANNEL_KEY_KEK=$$(openssl rand -base64 32)|" deploy/.env.stack; \
	  sed -i.bak "s|^GATEWAY_METRICS_TOKEN=$$|GATEWAY_METRICS_TOKEN=$$(openssl rand -hex 16)|" deploy/.env.stack; \
	  rm -f deploy/.env.stack.bak; \
	  echo "已生成 deploy/.env.stack（含随机密钥），请复核 MYSQL_ROOT_PASSWORD"; fi

stack-build: stack-env
	$(STACK) build

stack-up: stack-env
	$(STACK) up -d --build --wait --wait-timeout 300
	@echo "站点 http://localhost:$$(grep -E '^WEB_PORT=' deploy/.env.stack | cut -d= -f2)"

stack-down:
	$(STACK) down

stack-ps:
	$(STACK) ps

stack-logs:
	$(STACK) logs -f --tail=100

# 冒烟：迁移是否落库、三进程是否 ready、站点是否出首页、内网门是否拦得住。
# 断言与退出码都在 scripts/smoke.sh 里，这里只是对没有 bash 时的一层薄封装；
# 两边共用同一份实现，避免 Makefile 与脚本各写一套判据。
stack-smoke:
	@bash scripts/smoke.sh

ci: check
	@bash scripts/lint.sh
	@set -e; \
	if grep -rn '"encoding/json"' --include='*.go' . | grep -v 'internal/jsonx'; then \
		echo 'FAIL: encoding/json is only allowed inside internal/jsonx'; exit 1; \
	fi; \
	hits="$$(grep -rn 'context.Background()' --include='*.go' internal/ cmd/ || true)"; \
	bad="$$(printf '%s\n' "$$hits" | grep -v '^cmd/' | grep -v '^$$' || true)"; \
	if [ -n "$$bad" ]; then echo 'FAIL: context.Background() outside cmd/'; echo "$$bad"; exit 1; fi; \
	if grep -rn 'QuantumNous' --include='*.go' --include='*.sql' .; then \
		echo 'FAIL: new-api source string in Go/SQL — do not copy AGPL code (errata E-23)'; exit 1; \
	fi; \
	echo 'check: grep assertions passed'
