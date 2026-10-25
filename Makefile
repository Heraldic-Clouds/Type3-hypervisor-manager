# Type3 developer commands. Requires Go (go.mod pins go 1.27.3; older local
# toolchains download it automatically with GOTOOLCHAIN=auto).

BIN      := bin/type3
PKG      := github.com/Heraldic-Clouds/Type3-hypervisor-manager
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -s -w -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)
export GOTOOLCHAIN ?= auto

.PHONY: all build run test test-race test-integration cover vet fmt lint tidy check clean gen-key client-cert smoke

all: check build

build: ## Build the server binary
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/type3

run: build ## Run the server with .env
	./$(BIN) serve

test: ## Unit tests (mocked LXD, no host needed)
	go test ./...

test-race: ## Unit tests with the race detector
	go test -race ./...

test-integration: ## Real LXD tests; needs TYPE3_IT_LXD_ENDPOINT (see docs/testing.md)
	go test -tags integration -count=1 -v -timeout 60m ./test/integration/...

cover: ## Coverage report
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...
	go vet -tags integration ./test/...

fmt:
	gofmt -w cmd internal test

lint: vet ## gofmt check + vet
	@test -z "$$(gofmt -l cmd internal test)" || (echo "gofmt needed:"; gofmt -l cmd internal test; exit 1)

tidy:
	go mod tidy

check: lint test-race

gen-key: ## Make an API key: make gen-key NAME=admin ROLE=admin
	go run ./cmd/type3 gen-key $(or $(NAME),admin) $(or $(ROLE),admin)

client-cert: ## Create the LXD client certificate Type3 uses (config/type3-client.{crt,key})
	@mkdir -p config
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:secp384r1 -sha384 -days 825 -nodes \
		-keyout config/type3-client.key -out config/type3-client.crt -subj "/CN=type3"
	chmod 600 config/type3-client.key
	@echo "Trust it on each LXD host: lxc config trust add config/type3-client.crt --name type3"

smoke: ## Exercise a running server: TYPE3_API_KEY=... make smoke
	./scripts/api-smoke.sh

clean:
	rm -rf bin coverage.out
