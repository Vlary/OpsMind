# 云枢 OpsMind 常用命令。CI 使用 .github/workflows/ci.yml 中的等价命令。
.PHONY: dev-backend dev-web build test vet lint typecheck gen-api smoke seed db-up db-down docker-build

dev-backend:
	go run ./cmd/aiops

dev-web:
	pnpm -C web dev

db-up:
	docker compose up -d --wait db

db-down:
	docker compose down

build:
	go build -o bin/aiops ./cmd/aiops
	pnpm -C web build

vet:
	go vet ./...

test:
	go test ./...

typecheck:
	pnpm -C web typecheck

lint:
	pnpm -C web lint

gen-api:
	pnpm -C web gen:api

smoke:
	go run ./cmd/smoke

# 播种演示数据（两台示例服务器 + 指向假 LLM 的供应商 + 演示会话）。
# ADMIN_PASSWORD 缺省时可用 SEED_BASE_URL / SEED_LLM_BASE_URL 覆盖目标；
# CHAT=1 需要先启动假 LLM：node web/e2e/fixtures/fake-llm.mjs
seed:
	go run ./cmd/seed -password "$${ADMIN_PASSWORD}" $$(if [ -n "$${SEED_BASE_URL}" ]; then printf -- '-base-url %s' "$${SEED_BASE_URL}"; fi) $$(if [ -n "$${SEED_LLM_BASE_URL}" ]; then printf -- '-llm-base-url %s' "$${SEED_LLM_BASE_URL}"; fi) $$(if [ "$${CHAT}" = "1" ]; then printf -- '-chat'; fi)

docker-build:
	docker build -t aiops:local .
