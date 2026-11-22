.PHONY: help run build web web-dev tidy lint fmt test cover up down logs ps

help:
	@echo "make up      启动 MySQL/Redis/RabbitMQ"
	@echo "make run     启动服务（默认单节点）"
	@echo "make build   编译到 bin/imchat"
	@echo "make web     构建前端到 web/index.html（单文件产物）"
	@echo "make web-dev 启动前端开发服务器（:5173，代理到 :8080）"
	@echo "make lint    go vet 静态检查"
	@echo "make fmt     格式化 ./cmd、./internal 与 ./test"
	@echo "make test    运行全部单测"
	@echo "make cover   单测并输出总覆盖率"
	@echo "make tidy    go mod tidy"
	@echo "make down    停止依赖容器"
	@echo "make ps      查看依赖容器状态"

run:
	go run ./cmd/server -c config/config.yaml -node 1

build:
	go build -trimpath -o bin/imchat ./cmd/server

# 前端源码在 web/app/，构建产物是自包含的 web/index.html。
# 后端不托管它：产物由 nginx / 静态站点发布，服务只提供 API 与 WebSocket。
web:
	cd web && npm install --no-audit --no-fund && npm run build

web-dev:
	cd web && npm run dev

tidy:
	go mod tidy

lint:
	go vet ./...
	gofmt -l ./cmd ./internal ./test

# -count=1 关掉测试结果缓存：本机没有 MySQL/Redis 时大量用例走 t.Skip，
# 缓存会把「依赖起好后本该真跑」的那次复用成 skip，看日志根本分不清。
test:
	go test -count=1 ./...

# 覆盖率产物是 coverage.out，已被 .gitignore 的 *.out 覆盖，跑完 git status 仍是干净的。
cover:
	go test -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -n 1

# 只格式化 lint 检查的那三个目录，避免 -w 顺手改到 pkg 之外的文件。
fmt:
	gofmt -w ./cmd ./internal ./test

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

logs:
	docker compose -f deploy/docker-compose.yml logs -f

ps:
	docker compose -f deploy/docker-compose.yml ps
