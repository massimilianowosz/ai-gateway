.PHONY: build run test test-short test-integration lint verify fmt clean docker docker-compose migrate-up migrate-down coverage update-prices test-cache test-state build-push deploy-restart

BINARY_NAME=ubiquum
BUILD_DIR=bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"
REGISTRY ?= registry.wosz.it
IMAGE_NAME ?= $(REGISTRY)/ubiquum-ai-gateway
TAG ?= latest
PLATFORM ?= linux/amd64
KUBE_NAMESPACE ?= ubiquum
RELEASE_NAME ?= ai-router

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/gateway

run:
	go run ./cmd/gateway serve -config gateway.yaml

test:
	go test -race -coverprofile=coverage.out ./...

test-short:
	go test -short -race ./...

test-integration:
	go test -race -tags=integration ./test/integration/...

lint:
	golangci-lint run ./...

verify:
	go test ./...
	go test -race ./...
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping lint"; \
	fi

fmt:
	gofmt -s -w .
	goimports -w .

clean:
	rm -rf $(BUILD_DIR) coverage.out

docker:
	docker build -t ubiquum-ai-gateway:$(VERSION) .

# Build for production (linux/amd64) and push to registry
build-push:
	docker buildx build --platform $(PLATFORM) --load -t $(IMAGE_NAME):$(TAG) .
	docker push $(IMAGE_NAME):$(TAG)

# Restart gateway pods in production
deploy-restart:
	kubectl rollout restart deployment/$(RELEASE_NAME)-gateway -n $(KUBE_NAMESPACE)
	kubectl rollout status deployment/$(RELEASE_NAME)-gateway -n $(KUBE_NAMESPACE) --timeout=120s

# Build, push, and restart in one step
deploy: build-push deploy-restart

# Cross-compile CLI for client distribution
RELEASE_DIR=dist
release-cli:
	@echo "Building ubiquum CLI $(VERSION)..."
	@mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(RELEASE_DIR)/ubiquum-darwin-arm64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(RELEASE_DIR)/ubiquum-darwin-amd64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(RELEASE_DIR)/ubiquum-linux-amd64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(RELEASE_DIR)/ubiquum-linux-arm64 ./cmd/gateway
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(RELEASE_DIR)/ubiquum-windows-amd64.exe ./cmd/gateway
	@cd $(RELEASE_DIR) && shasum -a 256 ubiquum-* > checksums.txt
	@echo "Release binaries in $(RELEASE_DIR)/"
	@ls -lh $(RELEASE_DIR)/

docker-compose:
	docker compose up -d

migrate-up:
	go run ./cmd/gateway migrate up

migrate-down:
	go run ./cmd/gateway migrate down

coverage:
	go tool cover -html=coverage.out -o coverage.html

update-prices:
	./scripts/update_prices.sh
	git add internal/pricing/model_prices.json
	git commit -m "pricing: update model prices from upstream"
	git push

test-cache:
	UBIQUUM_API_KEY=sk-ubq-dddf9a97f3a59143ea547a9c2f3be5300d3f181ec5066476 \
	./test/cache/cache_bench_suite.sh --mock

test-state:
	python3 test/benchmarks/hivestate_multidomain_bench.py \
		--api-key sk-ubq-dddf9a97f3a59143ea547a9c2f3be5300d3f181ec5066476

.DEFAULT_GOAL := build
