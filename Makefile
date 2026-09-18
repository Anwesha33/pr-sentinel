SHELL := /bin/bash
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile every binary
	go build ./...

.PHONY: test
test: ## Run unit tests
	go test ./... -count=1

.PHONY: lint
lint: ## gofmt + go vet
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run gofmt -w ." && exit 1)
	go vet ./...

.PHONY: up
up: ## Start the full stack (postgres, redis, kafka, api, 2 workers)
	$(COMPOSE) up -d --build
	@echo "api on http://localhost:8080 — try: make submit PR=https://github.com/owner/repo/pull/1"

.PHONY: down
down: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail worker logs
	$(COMPOSE) logs -f worker

.PHONY: infra
infra: ## Start only the backing services, for running api/worker from source
	$(COMPOSE) up -d postgres redis kafka

.PHONY: submit
submit: ## Queue a review: make submit PR=https://github.com/owner/repo/pull/1
	@test -n "$(PR)" || (echo "usage: make submit PR=<pull request url>" && exit 1)
	curl -sS -X POST localhost:8080/v1/reviews \
		-H 'content-type: application/json' \
		-d '{"repo_url":"$(PR)"}' | python3 -m json.tool

.PHONY: status
status: ## Show a job: make status JOB=<uuid>
	@test -n "$(JOB)" || (echo "usage: make status JOB=<uuid>" && exit 1)
	curl -sS localhost:8080/v1/reviews/$(JOB) | python3 -m json.tool

.PHONY: trace
trace: ## Show the agent's tool calls: make trace JOB=<uuid>
	@test -n "$(JOB)" || (echo "usage: make trace JOB=<uuid>" && exit 1)
	curl -sS localhost:8080/v1/reviews/$(JOB)/steps | python3 -m json.tool

.PHONY: eval
eval: ## Score the agent against the planted-defect fixtures
	go run ./cmd/evalrunner -out eval/report.json

.PHONY: fixtures
fixtures: ## Regenerate eval/fixtures/*/pr.diff from base/ and head/
	@bash eval/regen-diffs.sh
