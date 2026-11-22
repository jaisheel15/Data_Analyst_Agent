.PHONY: setup up up-tracing down logs test test-go test-agent test-sandbox lint typecheck sandbox-image samples check-deploy eval eval-truth eval-test

setup: ## first-time setup: .env and data dir
	@test -f .env || (cp .env.example .env && echo "created .env: add your API key")
	@mkdir -p data

up: setup ## build and start everything on http://localhost:3000
	docker compose up -d --build
	@echo "open http://localhost:3000"

up-tracing: setup ## same as up, plus the Phoenix trace viewer on http://localhost:6006
	TRACING_OTLP_ENDPOINT=http://phoenix:6006/v1/traces docker compose --profile tracing up -d --build
	@echo "app: http://localhost:3000   traces: http://localhost:6006"

down:
	docker compose down

logs:
	docker compose logs -f api agent

sandbox-image: ## image used by the Go sandbox tests
	docker build -q -t daa-sandbox:dev sandbox

test: test-sandbox test-agent test-go ## all tests (needs Docker)

test-go: sandbox-image
	cd api && go test -race -count=1 -timeout 300s ./...

test-agent:
	cd agent && uv run pytest -q

test-sandbox:
	cd sandbox && uv run pytest -q

lint:
	cd api && go vet ./... && (command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed, skipped (go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0)")
	cd agent && uv run ruff check .

typecheck:
	cd web && npx nuxt typecheck

samples: ## regenerate the sample datasets
	uv run --project sandbox python scripts/make_samples.py

check-deploy: ## validate the production compose file and Caddyfile
	DOMAIN=example.com docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml config -q
	docker run --rm -e DOMAIN=example.com -v $(CURDIR)/deploy/Caddyfile:/etc/caddy/Caddyfile:ro caddy:2-alpine caddy validate --config /etc/caddy/Caddyfile

PROVIDER ?= deepseek
MODEL ?= deepseek-flash

eval: ## run the eval suite on a running stack: make eval PROVIDER=deepseek MODEL=deepseek-flash
	uv run --project evals python evals/run.py --provider $(PROVIDER) --model $(MODEL)

eval-truth: ## recompute expected answers from the datasets
	uv run --project evals python evals/build_ground_truth.py

eval-test: ## unit tests for the scoring code
	cd evals && uv run pytest -q
