SHELL := /bin/sh
.DEFAULT_GOAL := help
COMPOSE := docker compose

.PHONY: help standard up deploy demo test test-standard down status

help:
	@echo "standard       Start the Clojure service     up      Start the full demo"
	@echo "demo           Run live scenarios            test    Run the isolated test suite"
	@echo "test-standard  Test the Clojure implementation"
	@echo "deploy         Deploy to this host           down    Stop services and retain volumes"
	@echo "status         Show service status"

standard:
	$(COMPOSE) up -d --build --wait clojure
	@echo "curl -s -H 'Content-Type: application/json' -d '{\"user_id\":\"alice\"}' http://$$(docker compose port clojure 8080)/v1/check"

up deploy:
	$(COMPOSE) up -d --build --wait
	@echo "Console http://$$(docker compose port console 8080)/"
	@echo "Clojure API http://$$(docker compose port clojure 8080)/v1/check"
	@echo "Go cluster API http://$$(docker compose port gateway 8080)/v1/check"

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
