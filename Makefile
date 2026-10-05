SHELL := /bin/bash
GO     ?= go
UV     ?= uv
DC     := docker compose -f deploy/docker-compose.yml

.PHONY: help build test contract-test fmt vet dev-up up down logs clean

help: ## 列出可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 构建三个 Go 二进制到 bin/（chronotope-api / -worker / -executor）
	mkdir -p bin
	$(GO) build -o bin/chronotope-api ./cmd/api
	$(GO) build -o bin/chronotope-worker ./cmd/worker
	$(GO) build -o bin/chronotope-executor ./cmd/executor

test: ## Go 单元测试 + 契约测试（test/contract）
	$(GO) test ./...

contract-test: test ## Go 契约测试 + harness 侧 /runs 契约测试
	cd harness && $(UV) run pytest -q

fmt: ## gofmt 全量格式化
	$(GO) fmt ./...

vet: ## go vet 静态检查
	$(GO) vet ./...

dev-up: ## 仅启动基础设施（postgres / restate / minio / litellm）
	$(DC) up -d postgres restate minio litellm

up: ## 全量构建并启动（api/worker/executor/harness + 基础设施；web 属 W4 profile）
	$(DC) up -d --build

down: ## 停止全部容器
	$(DC) down

logs: ## 跟踪全部容器日志
	$(DC) logs -f

clean: ## 清理构建产物
	rm -rf bin/
