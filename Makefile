.PHONY: help run build tidy lint up down logs ps test

help:
	@echo "make up      启动 MySQL/Redis/RabbitMQ"
	@echo "make run     启动服务（默认单节点）"
	@echo "make build   编译到 bin/imchat"
	@echo "make lint    go vet 静态检查"
	@echo "make down    停止依赖容器"

run:
	go run ./cmd/server -c config/config.yaml -node 1

build:
	go build -trimpath -ldflags "-s -w" -o bin/imchat ./cmd/server

tidy:
	go mod tidy

lint:
	go vet ./...
	gofmt -l ./cmd ./internal

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

logs:
	docker compose -f deploy/docker-compose.yml logs -f

ps:
	docker compose -f deploy/docker-compose.yml ps
