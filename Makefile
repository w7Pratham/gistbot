GO ?= go

.PHONY: build test vet test-race

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

test-race:
	$(GO) test -race ./...
