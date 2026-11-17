.PHONY: help run build tidy lint fmt test cover up down logs ps

# 版本信息注入。git describe 在有 tag 时给 v0.1.0，tag 之后又有提交给
# v0.1.0-3-gabc1234，工作区有未提交改动则追加 -dirty。
# 取不到 git（源码包解压、CI 浅克隆没有 tag）时退回 dev/none，而不是让
# build 直接失败 —— 版本号是诊断信息，不该成为编译的前置条件。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X imchat/internal/buildinfo.Version=$(VERSION) -X imchat/internal/buildinfo.Commit=$(COMMIT)

help:
	@echo "make up      启动 MySQL/Redis/RabbitMQ"
	@echo "make run     启动服务（默认单节点）"
	@echo "make build   编译到 bin/imchat（注入 git describe 的版本号）"
	@echo "make lint    go vet 静态检查"
	@echo "make fmt     格式化 ./cmd 与 ./internal"
	@echo "make test    运行全部单测"
	@echo "make cover   单测并输出总覆盖率"
	@echo "make tidy    go mod tidy"
	@echo "make down    停止依赖容器"
	@echo "make ps      查看依赖容器状态"

run:
	go run ./cmd/server -c config/config.yaml -node 1

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/imchat ./cmd/server

tidy:
	go mod tidy

lint:
	go vet ./...
	gofmt -l ./cmd ./internal

# -count=1 关掉测试结果缓存：本机没有 MySQL/Redis 时大量用例走 t.Skip，
# 缓存会把「依赖起好后本该真跑」的那次复用成 skip，看日志根本分不清。
test:
	go test -count=1 ./...

# 覆盖率产物是 coverage.out，已被 .gitignore 的 *.out 覆盖，跑完 git status 仍是干净的。
cover:
	go test -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -n 1

# 只格式化 lint 检查的那两个目录，避免 -w 顺手改到 pkg 之外的文件。
fmt:
	gofmt -w ./cmd ./internal

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

logs:
	docker compose -f deploy/docker-compose.yml logs -f

ps:
	docker compose -f deploy/docker-compose.yml ps
