VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/asaleem9/routeparity/internal/cli.version=$(VERSION)

.PHONY: all build test test-race lint fmt golden tidy clean

all: lint test build

build: ## Build the routeparity binary into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/routeparity ./cmd/routeparity

test: ## Run unit and golden tests
	go test ./...

test-race: ## Run tests with the race detector and coverage
	go test -race -cover ./...

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format the code
	gofmt -s -w .

golden: ## Regenerate golden files; review the diff before committing
	go test ./internal/rules ./internal/cli -update

tidy: ## Tidy go.mod and go.sum
	go mod tidy

clean:
	rm -rf bin dist
