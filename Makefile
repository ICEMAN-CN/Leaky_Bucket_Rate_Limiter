SHELL := /bin/sh
.DEFAULT_GOAL := help
COMPOSE := docker compose

.PHONY: help standard up deploy demo test test-standard down status

help:
	@echo "standard  Clojure 答卷      up      完整操作台"
	@echo "demo      运行真实场景      test    全套隔离测试"
	@echo "deploy    当前主机部署      down    停止并保留卷"
	@echo "status    查看服务状态"

standard:
	$(COMPOSE) up -d --build --wait clojure
	@echo "curl -s -H 'Content-Type: application/json' -d '{\"user_id\":\"alice\"}' http://$$(docker compose port clojure 8080)/v1/check"

up deploy:
	$(COMPOSE) up -d --build --wait
	@echo "操作台 http://$$(docker compose port console 8080)/"
	@echo "标准版 http://$$(docker compose port clojure 8080)/v1/check"
	@echo "Go 集群 http://$$(docker compose port gateway 8080)/v1/check"

demo:
	$(COMPOSE) exec -T console console demo

test-standard:
	$(COMPOSE) -f compose.yaml -f compose.test.yaml run --rm --build --no-deps standard-tests

test:
	@sh scripts/test.sh

down:
	$(COMPOSE) down

status:
	$(COMPOSE) ps
