GO ?= go

.PHONY: build test vet test-race

build:
	@mkdir -p bin
	$(GO) build -o bin/gistbot ./cmd/gistbot

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

test-race:
	$(GO) test -race ./...
