# 常用任务入口。变量都可覆盖：make server PORT=9000
SHELL := /bin/bash
GO    ?= go
PORT  ?= 8083
BIND  ?= 127.0.0.1
DB    ?= $(CURDIR)/.build/dev.db

.DEFAULT_GOAL := help
.PHONY: help server server-run rhai apk web api-origin clean fmt test

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

server: ## 编译后端 → server/chat-server/chat-server
	cd server/chat-server && $(GO) build -trimpath -ldflags="-s -w" -o chat-server .

server-run: server ## 本地起后端（读 .env，DB 落在 .build/dev.db）
	@mkdir -p .build
	@set -a; [ -f .env ] && . ./.env; set +a; \
	CHAT_BIND=$(BIND) CHAT_PORT=$(PORT) CHAT_DB=$(DB) server/chat-server/chat-server

rhai: ## 编译沙箱执行器 → server/rhai-runner/target/release/rhai-runner
	cd server/rhai-runner && cargo build --release

apk: ## 打安卓包（API 域名取 API_ORIGIN，默认 example.com）
	cd client/android && bash build.sh $(if $(API_ORIGIN),--api-origin $(API_ORIGIN),)

api-origin: ## 改前端接口地址：make api-origin API_ORIGIN=https://api.your.host
	@test -n "$(API_ORIGIN)" || { echo "用法: make api-origin API_ORIGIN=https://api.your.host"; exit 1; }
	python3 tools/set-api-origin.py "$(API_ORIGIN)"

web: ## 把静态前端发布到远端（HOST=root@server WEB_ROOT=/var/www/chat）
	cd server/chat-server && HOST=$(HOST) bash ops/deploy-web.sh ../../client/android/app/assets

fmt test: ## 格式化 / 跑测试
fmt:
	cd server/chat-server && gofmt -w . && $(GO) vet ./...
test:
	cd server/chat-server && $(GO) test ./...

clean: ## 清掉构建产物
	rm -rf server/chat-server/chat-server server/chat-server/.build .build \
	       client/android/.work client/android/dist server/rhai-runner/target
