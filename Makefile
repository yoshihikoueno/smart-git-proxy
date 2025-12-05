GO ?= mise exec -- go
BIN := bin/smart-git-proxy
PKG := ./...

.PHONY: all build lint test fmt tidy

all: build

build:
	$(GO) build -o $(BIN) ./cmd/proxy

lint:
	golangci-lint run ./...

test:
	$(GO) test $(PKG)

fmt:
	gofmt -w .

tidy:
	$(GO) mod tidy

