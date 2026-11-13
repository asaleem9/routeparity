VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/asaleem9/routeparity/internal/cli.version=$(VERSION)

ECHO_IMAGE ?= routeparity-echo:dev
GOARCH := $(shell go env GOARCH)

.PHONY: all build echo-image test test-race e2e lint fmt golden tidy clean

all: lint test build

build: ## Build the routeparity binary into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/routeparity ./cmd/routeparity

echo-image: ## Build the lab's echo backend image for this machine's architecture
	mkdir -p bin/echo-image
	GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
		-o bin/echo-image/routeparity-echo-$(GOARCH) ./cmd/routeparity-echo
	docker build -q -t $(ECHO_IMAGE) -f hack/echo.Dockerfile bin/echo-image

test: ## Run unit and golden tests
	go test ./...

test-race: ## Run tests with the race detector and coverage
	go test -race -cover ./...

e2e: echo-image ## Run the lab against a real kind cluster; needs Docker
	go test -tags e2e -count=1 -timeout 20m -v ./e2e/...

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
